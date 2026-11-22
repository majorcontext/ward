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
)

// ErrNoMergeBase means that the first-parent chain of the branch never meets the first-parent chain of the target ref.
var ErrNoMergeBase = errors.New("land: the branch has no merge base with the ref")

// Config says how ward reaches motte and signs its grants.
type Config struct {
	URL string
	Kid string
	Key ed25519.PrivateKey
	// ReadBudget is the time limit of one read of motte. Zero means DefaultReadBudget.
	ReadBudget time.Duration
	// BatchMax is the most lands that one check run judges. Zero means DefaultBatchMax, and 1 judges each land alone.
	BatchMax int
}

// Request names the branch to land and where.
type Request struct {
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
	return nil
}

// Wait returns when every queue has stopped. Call it after the context of Start ends.
func (s *Service) Wait() { s.wg.Wait() }

// Enqueue records a land in state queued and wakes the queue of its ref.
func (s *Service) Enqueue(ctx context.Context, req Request) (Land, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Land{}, err
	}
	l := Land{ID: hex.EncodeToString(b[:]), Repo: req.Repo, Ref: req.Ref, Branch: req.Branch, DeleteBranch: req.DeleteBranch, State: Queued, Checks: []CheckState{}}
	if err := s.store.InsertLand(ctx, l); err != nil {
		return Land{}, err
	}
	s.kick(Key{req.Repo, req.Ref})
	return l, nil
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
	for {
		ls, err := s.store.NextLands(s.ctx, k, s.batchMax())
		switch {
		case err != nil:
			if s.ctx.Err() == nil {
				s.log.Error("read the land queue", "repo", k.Repo, "ref", k.Ref, "error", err.Error())
			}
			s.retireNow(k)
			return
		case len(ls) == 0:
			if s.retire(k, wake) {
				return
			}
			continue
		}
		if err := s.step(k, ls); err != nil {
			if s.ctx.Err() == nil {
				s.log.Error("record the land", "land", ls[0].ID, "error", err.Error())
			}
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
		return s.resumeBatch(k, ls)
	}
	if ls[0].Call != nil && ls[0].State == Landing {
		ls = ls[:1]
	}
	if ls = ls[:min(len(ls), s.size(k))]; len(ls) > 1 {
		return s.runBatch(k, ls)
	}
	checkFailed, err := s.run(ls[0])
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
