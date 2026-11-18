// Package mirror keeps a one-way mirror from motte to external git remotes.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/majorcontext/motte/client"
)

const (
	hookPath = "/v1/hooks/motte"
	// DefaultDiskBudget is the default size of the clone cache.
	DefaultDiskBudget = 20 << 30
	// DefaultIdleTTL is the default idle time of a clone.
	DefaultIdleTTL = 168 * time.Hour
)

// Config is the configuration of the engine.
type Config struct {
	DataDir    string
	Motte      MotteConfig
	PublicURL  string
	HookSecret []byte
	Mirrors    []Entry
	// GitHubAPI is the API base URL for the GitHub App credential. Empty means DefaultGitHubAPI.
	GitHubAPI  string
	DiskBudget int64
	IdleTTL    time.Duration
	// SeedMaxObjects and SeedMaxBytes bound one push of a seed. Zero means the default.
	SeedMaxObjects int
	SeedMaxBytes   int64
	// Askpass is the program that git runs for the remote credential. It is the ward binary.
	Askpass string
}

// Options are the parts that a caller may replace.
type Options struct {
	Clock  Clock
	Logger *slog.Logger
	// Done is called after a unit of work ends: kind is batch, reconcile, catchup, reset or init, and mirror is empty
	// for work that belongs to no mirror. It must not block.
	Done func(kind, mirror string)
}

// Engine runs the mirror.
type Engine struct {
	cfg     Config
	store   Store
	motte   motteSigner
	git     *gitRunner
	clock   Clock
	log     *slog.Logger
	done    func(kind, mirror string)
	m       *metrics
	entries map[string]Entry
	apps    map[string]*appSource
	repos   repoLocks

	mu      sync.Mutex
	want    wants
	ctrl    chan struct{}
	workers map[string]*worker
	// pending holds the Reconcile requests that came before the worker of a mirror started.
	pending map[string]bool
	// seedStates holds StateSeeding or StateSeedRefused for each mirror that has one.
	seedStates map[string]string
}

type wants struct{ init, catchup, reset bool }

// New builds an engine over a state store.
func New(cfg Config, store Store, opts Options) *Engine {
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.DiskBudget == 0 {
		cfg.DiskBudget = DefaultDiskBudget
	}
	if cfg.SeedMaxObjects == 0 {
		cfg.SeedMaxObjects = DefaultSeedMaxObjects
	}
	if cfg.SeedMaxBytes == 0 {
		cfg.SeedMaxBytes = DefaultSeedMaxBytes
	}
	if cfg.IdleTTL == 0 {
		cfg.IdleTTL = DefaultIdleTTL
	}
	e := &Engine{
		cfg: cfg, store: store, clock: opts.Clock, log: opts.Logger, done: opts.Done,
		motte:   motteSigner{cfg: cfg.Motte, clock: opts.Clock},
		git:     &gitRunner{home: cfg.DataDir, askpass: cfg.Askpass, timeout: gitTimeout},
		m:       newMetrics(store, opts.Clock),
		entries: map[string]Entry{},
		apps:    map[string]*appSource{},
		repos:   repoLocks{m: map[string]*sync.Mutex{}},
		ctrl:    make(chan struct{}, 1),
		workers: map[string]*worker{},
		pending: map[string]bool{},

		seedStates: map[string]string{},
	}
	for _, en := range cfg.Mirrors {
		e.entries[en.Name] = en
		if en.GitHubApp != nil {
			e.apps[en.Name] = newAppSource(*en.GitHubApp, cfg.GitHubAPI, opts.Clock)
		}
	}
	return e
}

// Registry is the Prometheus registry of the engine.
func (e *Engine) Registry() *prometheus.Registry { return e.m.reg }

// Mirror returns the entry with the name.
func (e *Engine) Mirror(name string) (Entry, bool) {
	en, ok := e.entries[name]
	return en, ok
}

// Mirrors returns the entries in configuration order.
func (e *Engine) Mirrors() []Entry { return e.cfg.Mirrors }

func (e *Engine) notify(kind, mirror string) {
	if e.done != nil {
		e.done(kind, mirror)
	}
}

func (e *Engine) request(w func(*wants)) {
	e.mu.Lock()
	w(&e.want)
	e.mu.Unlock()
	select {
	case e.ctrl <- struct{}{}:
	default:
	}
}

// RequestReset asks for a Reset. It returns at once.
func (e *Engine) RequestReset() { e.request(func(w *wants) { w.reset = true }) }

// RequestReconcile asks the worker of a mirror for a Reconcile. It returns at once.
func (e *Engine) RequestReconcile(name string) bool {
	e.mu.Lock()
	w := e.workers[name]
	e.mu.Unlock()
	if w == nil {
		if _, ok := e.entries[name]; !ok {
			return false
		}
		e.mu.Lock()
		e.pending[name] = true
		e.mu.Unlock()
		return true
	}
	w.requestReconcile()
	return true
}

// Run drives the engine until ctx ends. A failed step of the control work retries with the backoff of a failed action.
func (e *Engine) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	e.request(func(w *wants) { w.init = true })
	attempts := 0
	for {
		if err := e.control(ctx, &wg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			attempts++
			e.log.Warn("control step failed", "attempt", attempts, "error", sanitize(err))
			select {
			case <-ctx.Done():
				return nil
			case <-e.clock.After(backoff(attempts)):
			}
			continue
		}
		attempts = 0
		select {
		case <-ctx.Done():
			return nil
		case <-e.ctrl:
		}
	}
}

func (e *Engine) take(pick func(*wants) *bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := pick(&e.want)
	was := *p
	*p = false
	return was
}

func (e *Engine) control(ctx context.Context, wg *sync.WaitGroup) error {
	for ctx.Err() == nil {
		switch {
		case e.take(func(w *wants) *bool { return &w.init }):
			if err := e.initialize(ctx, wg); err != nil {
				e.request(func(w *wants) { w.init = true })
				return err
			}
		case e.take(func(w *wants) *bool { return &w.reset }):
			if err := e.reset(ctx); err != nil {
				e.request(func(w *wants) { w.reset = true })
				return err
			}
		case e.take(func(w *wants) *bool { return &w.catchup }):
			if err := e.catchUp(ctx); err != nil {
				e.request(func(w *wants) { w.catchup = true })
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

func (e *Engine) initialize(ctx context.Context, wg *sync.WaitGroup) error {
	if err := e.ensureSubscription(ctx); err != nil {
		return fmt.Errorf("subscription: %w", err)
	}
	e.startWorkers(ctx, wg)
	for _, en := range e.cfg.Mirrors {
		known, err := e.store.HasMirror(ctx, en.Name)
		if err != nil {
			return err
		}
		seedState := ""
		if en.Seed {
			if seedState, err = e.store.Seed(ctx, en.Name); err != nil {
				return err
			}
		}
		switch {
		case en.Seed && seedState != seedDone && (seedState == seedStarted || !known):
			e.workers[en.Name].requestSeed()
		case !known:
			e.workers[en.Name].requestReconcile()
		}
	}
	e.request(func(w *wants) { w.catchup = true })
	e.notify("init", "")
	return nil
}

func (e *Engine) startWorkers(ctx context.Context, wg *sync.WaitGroup) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, en := range e.cfg.Mirrors {
		if e.workers[en.Name] != nil {
			continue
		}
		w := newWorker(e, en)
		e.workers[en.Name] = w
		if e.pending[en.Name] {
			delete(e.pending, en.Name)
			w.requestReconcile()
		}
		wg.Go(func() { w.run(ctx) })
	}
}

func (e *Engine) hookURL() string { return e.cfg.PublicURL + hookPath }

// register creates the subscription and returns its id and cursor.
func (e *Engine) register(ctx context.Context) (id, cursor string, err error) {
	sub, err := e.motte.operator().CreateSubscription(ctx, subscriptionRequest(e.hookURL(), e.cfg.HookSecret))
	return sub.ID, sub.Cursor, err
}

// dropSubscriptions deletes every subscription that has the receiver URL of ward.
func (e *Engine) dropSubscriptions(ctx context.Context) error {
	c := e.motte.operator()
	subs, err := c.Subscriptions(ctx)
	if err != nil {
		return err
	}
	for _, s := range subs {
		if s.URL == e.hookURL() {
			if err := c.DeleteSubscription(ctx, s.ID); err != nil && !errors.Is(err, client.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) ensureSubscription(ctx context.Context) error {
	svc, err := e.store.Service(ctx)
	if err != nil {
		return err
	}
	subs, err := e.motte.operator().Subscriptions(ctx)
	if err != nil {
		return err
	}
	for _, s := range subs {
		if s.URL == e.hookURL() && s.ID == svc.SubscriptionID {
			return nil
		}
	}
	if err := e.dropSubscriptions(ctx); err != nil {
		return err
	}
	id, cursor, err := e.register(ctx)
	if err != nil {
		return err
	}
	if svc.EventsCursor != "" {
		cursor = svc.EventsCursor
	}
	return e.store.SetSubscription(ctx, id, cursor, false)
}

// reset replaces the subscription, clears last_seq, and asks every mirror for a Reconcile.
func (e *Engine) reset(ctx context.Context) error {
	if err := e.dropSubscriptions(ctx); err != nil {
		return err
	}
	id, cursor, err := e.register(ctx)
	if err != nil {
		return err
	}
	if err := e.store.SetSubscription(ctx, id, cursor, true); err != nil {
		return err
	}
	e.mu.Lock()
	for _, w := range e.workers {
		w.requestReconcile()
	}
	e.mu.Unlock()
	e.log.Info("reset done")
	e.notify("reset", "")
	return nil
}

func (e *Engine) wakeMirrors(names []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range names {
		if w := e.workers[n]; w != nil {
			w.wake()
		}
	}
}

// repoLocks holds one mutex for each repository text. A clone belongs to one repository.
type repoLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (r *repoLocks) get(repo string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[repo] == nil {
		r.m[repo] = &sync.Mutex{}
	}
	return r.m[repo]
}

// Stats returns the summary of every mirror that has state.
func (e *Engine) Stats(ctx context.Context) ([]Stats, error) { return e.store.Stats(ctx) }

// Refs returns the refs of a mirror after the ref name, in name order.
func (e *Engine) Refs(ctx context.Context, name, after string, limit int) ([]Ref, error) {
	return e.store.Refs(ctx, name, after, limit)
}
