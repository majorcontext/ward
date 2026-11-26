// Package land lands a motte branch on a ref through a per-ref queue. A ref only ever holds a tree that passed its checks.
package land

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/motte/grant"

	"github.com/majorcontext/ward/internal/check"
)

const (
	grantLife = 5 * time.Minute
	maxWalk   = 10000
	// MaxWait is the longest wait of a read of a land.
	MaxWait = 60 * time.Second
	// DefaultReadBudget is the time limit of one read of motte. A replica that opens its packs on first use needs more than
	// the 2 s default of the client. The limit stays below the 35 s that the client gives a land call.
	DefaultReadBudget = 10 * time.Second
	// DefaultBatchMax is the most lands that one check run judges when Config.BatchMax is zero.
	DefaultBatchMax = 8
	startBatch      = 2
	// sweepEvery is the time between two kicks of every queue that has lands.
	sweepEvery = time.Minute
	// storeBackoffMin and storeBackoffMax bound the wait before a worker retries a failed store operation.
	storeBackoffMin = time.Second
	storeBackoffMax = time.Minute
)

var tracer = otel.Tracer("github.com/majorcontext/ward/internal/land")

// span starts a child span of ctx. The function that it returns ends the span with err and attrs.
func span(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(err error, attrs ...attribute.KeyValue)) {
	ctx, sp := tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	return ctx, func(err error, attrs ...attribute.KeyValue) {
		sp.SetAttributes(attrs...)
		if err != nil {
			sp.AddEvent("exception", trace.WithAttributes(attribute.String("exception.type", fmt.Sprintf("%T", err))))
			sp.SetStatus(codes.Error, "error")
		}
		sp.End()
	}
}

// ErrNoMergeBase means that the first-parent chain of the branch never meets the first-parent chain of the target ref.
var ErrNoMergeBase = errors.New("land: the branch has no merge base with the ref")

// Config says how ward reaches motte and signs its grants.
type Config struct {
	URL string
	Kid string
	Key ed25519.PrivateKey
	// ReadBudget is the time limit of one read of motte. Zero means DefaultReadBudget.
	ReadBudget time.Duration
	// Refs lists the ref patterns that a land may name: a stored ref name, or a prefix that ends in *. Nil means main only.
	Refs []string
	// BatchMax is the most lands that one check run judges. Zero means DefaultBatchMax, and 1 judges each land alone.
	BatchMax int
}

// Request names the branch to land and where.
type Request struct {
	// ID is chosen by the caller. A repeat of an accepted ID answers the land that it made. Empty means ward chooses one.
	ID           string
	Repo         string
	Ref          string
	Branch       string
	DeleteBranch bool
}

// Service accepts lands and runs the queue of each (repo, ref).
type Service struct {
	cfg    Config
	store  Store
	checks *check.Manager
	clock  check.Clock
	log    *slog.Logger

	mu      sync.Mutex
	ctx     context.Context
	workers map[Key]chan struct{}
	sizes   map[Key]int
	changed chan struct{}
	wg      sync.WaitGroup
	// queued holds the time that each land of this process was accepted, for the span of its run.
	queued sync.Map
}

// waited returns the span attribute of the longest time that a land of ls has waited in this process, or none.
func (s *Service) waited(ls ...Land) []attribute.KeyValue {
	var longest time.Duration
	for _, l := range ls {
		if at, ok := s.queued.LoadAndDelete(l.ID); ok {
			longest = max(longest, s.clock.Now().Sub(at.(time.Time)))
		}
	}
	if longest == 0 {
		return nil
	}
	return []attribute.KeyValue{attribute.Float64("queue_wait_ms", float64(longest)/1e6)}
}

// New returns a service. A nil clock means the wall clock.
func New(cfg Config, store Store, checks *check.Manager, clock check.Clock, log *slog.Logger) *Service {
	if clock == nil {
		clock = check.RealClock()
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{cfg: cfg, store: store, checks: checks, clock: clock, log: log, workers: map[Key]chan struct{}{}, sizes: map[Key]int{}, changed: make(chan struct{})}
}

// Start resumes every land that has not ended. The queues stop when ctx ends.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	keys, err := s.store.UnfinishedKeys(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		s.kick(k)
	}
	s.wg.Add(1)
	go s.sweep(ctx)
	return nil
}

// sweep kicks every queue that has lands, so that a lost worker restarts.
func (s *Service) sweep(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-s.clock.After(sweepEvery):
		case <-ctx.Done():
			return
		}
		keys, err := s.store.UnfinishedKeys(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("sweep the land queues", "error", err.Error())
			}
			continue
		}
		for _, k := range keys {
			s.kick(k)
		}
	}
}

// Wait returns when every queue has stopped. Call it after the context of Start ends.
func (s *Service) Wait() { s.wg.Wait() }

// ErrIDInUse means that the ID of a request belongs to a land of another repository, ref or branch.
var ErrIDInUse = errors.New("land: the id belongs to another land")

// RefAllowed reports whether a pattern of the configuration names ref.
func (s *Service) RefAllowed(ref string) bool {
	refs := s.cfg.Refs
	if refs == nil {
		refs = []string{"main"}
	}
	for _, p := range refs {
		if prefix, wild := strings.CutSuffix(p, "*"); wild {
			if strings.HasPrefix(ref, prefix) {
				return true
			}
		} else if p == ref {
			return true
		}
	}
	return false
}

// Enqueue records a land in state queued and wakes the queue of its ref. A repeat of a request ID answers the land that the
// first request made, and ErrIDInUse answers a request that differs from it.
func (s *Service) Enqueue(ctx context.Context, req Request) (Land, error) {
	id := req.ID
	if id == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Land{}, err
		}
		id = hex.EncodeToString(b[:])
	} else if l, ok, err := s.Repeat(ctx, req); ok || err != nil {
		return l, err
	}
	l := Land{ID: id, Repo: req.Repo, Ref: req.Ref, Branch: req.Branch, DeleteBranch: req.DeleteBranch, State: Queued, Checks: []CheckState{}}
	ctx, end := span(ctx, "ward.land.enqueue", attribute.String("land_id", l.ID))
	s.queued.Store(l.ID, s.clock.Now())
	err := s.store.InsertLand(ctx, l)
	end(err)
	if err != nil {
		s.queued.Delete(l.ID)
		if req.ID != "" {
			if again, ok, rerr := s.Repeat(ctx, req); ok || rerr != nil {
				return again, rerr
			}
		}
		return Land{}, err
	}
	s.kick(Key{req.Repo, req.Ref})
	return l, nil
}

// Repeat returns the land that req.ID made when it exists and agrees with req.
func (s *Service) Repeat(ctx context.Context, req Request) (Land, bool, error) {
	l, ok, err := s.store.Land(ctx, req.ID)
	switch {
	case err != nil || !ok:
		return Land{}, false, err
	case l.Repo != req.Repo || l.Ref != req.Ref || l.Branch != req.Branch || l.DeleteBranch != req.DeleteBranch:
		return Land{}, true, ErrIDInUse
	}
	return l, true, nil
}

// Get returns a land. The baseline state is known, or the state of the land now when known is empty. With a positive wait,
// Get returns when the state differs from the baseline, or when the wait ends.
func (s *Service) Get(ctx context.Context, id string, wait time.Duration, known State) (Land, bool, error) {
	changed := s.signal()
	first, ok, err := s.store.Land(ctx, id)
	if err != nil || !ok {
		return first, ok, err
	}
	if known == "" {
		known = first.State
	}
	if wait <= 0 || first.State != known || first.State.Ended() {
		return first, true, nil
	}
	timeout := s.clock.After(min(wait, MaxWait))
	for {
		select {
		case <-changed:
			changed = s.signal()
		case <-timeout:
			return s.reread(ctx, id)
		case <-s.done():
			return s.reread(ctx, id)
		case <-ctx.Done():
			return first, true, ctx.Err()
		}
		cur, ok, err := s.store.Land(ctx, id)
		if err != nil || !ok || cur.State != known {
			return cur, ok, err
		}
	}
}

func (s *Service) done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx.Done()
}

func (s *Service) reread(ctx context.Context, id string) (Land, bool, error) {
	return s.store.Land(ctx, id)
}

func (s *Service) signal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Service) notify() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

// kick starts the worker of a key, or wakes it when it runs.
func (s *Service) kick(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil {
		return
	}
	if wake, ok := s.workers[k]; ok {
		select {
		case wake <- struct{}{}:
		default:
		}
		return
	}
	wake := make(chan struct{}, 1)
	s.workers[k] = wake
	s.wg.Add(1)
	go s.work(k, wake)
}

// retire removes the worker of k unless a land arrived since it looked. It returns true when the worker must stop.
func (s *Service) retire(k Key, wake chan struct{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-wake:
		return false
	default:
		delete(s.workers, k)
		return true
	}
}

func (s *Service) work(k Key, wake chan struct{}) {
	defer s.wg.Done()
	backoff := time.Duration(0)
	for {
		ls, err := s.store.NextLands(s.ctx, k, s.batchMax())
		switch {
		case err != nil:
			if s.ctx.Err() == nil {
				s.log.Error("read the land queue", "repo", k.Repo, "ref", k.Ref, "error", err.Error())
			}
		case len(ls) == 0:
			if s.retire(k, wake) {
				return
			}
			backoff = 0
			continue
		default:
			if err = s.step(k, ls); err != nil && s.ctx.Err() == nil {
				s.log.Error("record the land", "land", ls[0].ID, "error", err.Error())
			}
		}
		if s.ctx.Err() != nil {
			s.retireNow(k)
			return
		}
		if err == nil {
			backoff = 0
			continue
		}
		backoff = min(max(2*backoff, storeBackoffMin), storeBackoffMax)
		select {
		case <-s.clock.After(backoff):
		case <-s.ctx.Done():
			s.retireNow(k)
			return
		}
	}
}

func (s *Service) batchMax() int { return cmp.Or(s.cfg.BatchMax, DefaultBatchMax) }

// size returns the number of lands that the next check run of k may judge.
func (s *Service) size(k Key) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return min(cmp.Or(s.sizes[k], startBatch), s.batchMax())
}

// adapt grows the batch of k by one after a pass and halves it after a failed check.
func (s *Service) adapt(k Key, passed bool) {
	n := s.size(k)
	if passed {
		n++
	} else {
		n /= 2
	}
	s.mu.Lock()
	s.sizes[k] = min(max(n, 1), s.batchMax())
	s.mu.Unlock()
}

// step runs the oldest lands of k, as many as the batch size allows, to their end or back to the queue.
func (s *Service) step(k Key, ls []Land) error {
	if ls[0].Call != nil && ls[0].State == Landing && ls[0].Call.ID != "" {
		ctx, end := span(s.ctx, "ward.land.batch", attribute.Int("size", len(ls)), attribute.Bool("resumed", true))
		err := s.resumeBatch(ctx, k, ls)
		end(err)
		return err
	}
	if ls[0].Call != nil && ls[0].State == Landing {
		ls = ls[:1]
	}
	if ls = ls[:min(len(ls), s.size(k))]; len(ls) > 1 {
		ctx, end := span(s.ctx, "ward.land.batch", append(s.waited(ls...), attribute.Int("size", len(ls)))...)
		err := s.runBatch(ctx, k, ls)
		end(err)
		return err
	}
	ctx, end := span(s.ctx, "ward.land.run", append(s.waited(ls[0]), attribute.String("land_id", ls[0].ID))...)
	checkFailed, err := s.run(ctx, ls[0])
	end(err, attribute.Bool("check_failed", checkFailed))
	if err == nil && s.ctx.Err() == nil {
		s.adapt(k, !checkFailed)
	}
	return err
}

func (s *Service) retireNow(k Key) {
	s.mu.Lock()
	delete(s.workers, k)
	s.mu.Unlock()
}

// client returns a client of motte. Only the read client sets a budget, because a budget also replaces the land limit.
func (s *Service) client(scope grant.Scope, repo string) *client.Client {
	var opts []client.Option
	if scope == grant.ScopeRead {
		opts = append(opts, client.WithBudget(cmp.Or(s.cfg.ReadBudget, DefaultReadBudget)))
	}
	return client.New(s.cfg.URL, func(context.Context) (string, error) {
		return grant.Signer{Kid: s.cfg.Kid, Key: s.cfg.Key}.Mint(grant.Claims{Scope: scope, Repo: repo, Subject: "ward", Exp: s.clock.Now().Add(grantLife).Unix()})
	}, opts...)
}

func firstParent(ctx context.Context, c *client.Client, sha string) (string, error) {
	obj, err := c.Object(ctx, sha)
	if err != nil {
		return "", err
	}
	if obj.Type != "commit" {
		return "", fmt.Errorf("%w: %s is not a commit", ErrNoMergeBase, sha)
	}
	head, _, _ := bytes.Cut(obj.Data, []byte("\n\n"))
	for line := range strings.Lines(string(head)) {
		if p, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "parent "); ok {
			return p, nil
		}
	}
	return "", nil
}

// mergeBase walks both first-parent chains one commit at a time. The first commit that one walk reaches and the other
// has already seen is the newest common commit.
func mergeBase(ctx context.Context, c *client.Client, tip, head string) (string, error) {
	seenTip, seenHead := map[string]bool{}, map[string]bool{}
	for n := 0; (tip != "" || head != "") && n < maxWalk; n++ {
		var err error
		if tip != "" {
			if seenHead[tip] {
				return tip, nil
			}
			seenTip[tip] = true
			if tip, err = firstParent(ctx, c, tip); err != nil {
				return "", err
			}
		}
		if head != "" {
			if seenTip[head] {
				return head, nil
			}
			seenHead[head] = true
			if head, err = firstParent(ctx, c, head); err != nil {
				return "", err
			}
		}
	}
	return "", ErrNoMergeBase
}
