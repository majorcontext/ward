package check

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/majorcontext/ward/runnerproto"
)

const resendMargin = 10 * time.Minute

var (
	// ErrInvalid means that a posted result breaks a rule of the body.
	ErrInvalid = errors.New("check: invalid result")
	// ErrUnknownRequest means that no request has the posted request_id.
	ErrUnknownRequest = errors.New("check: unknown request")
	// ErrOtherRepo means that the request belongs to another repository.
	ErrOtherRepo = errors.New("check: request of another repository")
	// ErrNotRegistered means that the poster is not a runner that leased the request of a pull repository.
	ErrNotRegistered = errors.New("check: runner not registered for the repository")
)

// State is the state of one check for one tree.
type State string

const (
	Pending State = "pending"
	Pass    State = "pass"
	Fail    State = "fail"
	Flaky   State = "flaky"
)

// Verdict is the state of one check, and the failing result when the state is Fail.
type Verdict struct {
	State  State
	Result Result
}

// Posted is the body that a runner posts.
type Posted = runnerproto.Result

// Manager keeps the results and sends the check requests. It has no poll: a result wakes the waiters.
type Manager struct {
	store   Store
	runners map[string]Runner
	wake    func()
	clock   Clock
	log     *slog.Logger

	mu      sync.Mutex
	changed chan struct{}
	ctx     context.Context
	wg      sync.WaitGroup
}

// New returns a manager. wake tells the polling runners that a request is open; it may be nil. A nil clock means the wall clock.
func New(store Store, runners []Runner, wake func(), clock Clock, log *slog.Logger) *Manager {
	if clock == nil {
		clock = RealClock()
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Manager{store: store, runners: map[string]Runner{}, wake: wake, clock: clock, log: log, changed: make(chan struct{})}
	for _, r := range runners {
		m.runners[r.Repo] = r
	}
	return m
}

// Start resumes every open request. The deliveries stop when ctx ends.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	open, err := m.store.OpenRequests(ctx)
	if err != nil {
		return err
	}
	for _, r := range open {
		m.spawn(r)
	}
	return nil
}

// Wait returns when every delivery has stopped. Call it after the context of Start ends.
func (m *Manager) Wait() { m.wg.Wait() }

// Changed returns a channel that closes at the next result. Take it before reading the state that the result changes.
func (m *Manager) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

func (m *Manager) broadcast() {
	m.mu.Lock()
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
}

// Runner returns the runner of a repository.
func (m *Manager) Runner(repo string) (Runner, bool) {
	r, ok := m.runners[repo]
	return r, ok
}

// Mark returns the position after the newest result. A fail counts for a land only when it is after its mark.
func (m *Manager) Mark(ctx context.Context) (int64, error) { return m.store.MaxResultID(ctx) }

// Verdict reads the state of check c for tree. A pass is final. A fail counts only when it is after mark.
func (m *Manager) Verdict(ctx context.Context, repo, tree string, c Check, mark int64) (Verdict, error) {
	r, ok := m.runners[repo]
	if !ok {
		return Verdict{}, ErrNoRunner
	}
	results, err := m.store.Results(ctx, repo, tree, c.Hash(), r.Env)
	if err != nil {
		return Verdict{}, err
	}
	var pass, fail bool
	var failed Result
	for _, res := range results {
		if res.Status == StatusPass {
			pass = true
			continue
		}
		fail = true
		if res.ID > mark && failed.ID == 0 {
			failed = res
		}
	}
	switch {
	case pass && fail:
		return Verdict{State: Flaky}, nil
	case pass:
		return Verdict{State: Pass}, nil
	case failed.ID != 0:
		return Verdict{State: Fail, Result: failed}, nil
	}
	return Verdict{State: Pending}, nil
}

// Ensure makes sure that a request for check c of tree is open, and starts its delivery when it opens one.
func (m *Manager) Ensure(ctx context.Context, repo, tree, commit string, c Check) error {
	r, ok := m.runners[repo]
	if !ok {
		return ErrNoRunner
	}
	id, err := newID()
	if err != nil {
		return err
	}
	req, created, err := m.store.OpenRequest(ctx, Request{ID: id, Repo: repo, Tree: tree, Commit: commit, Check: c, Hash: c.Hash(), Env: r.Env, Open: true})
	if err != nil {
		return err
	}
	if created {
		m.spawn(req)
	}
	return nil
}

// Post records a result that the runner kid posted for a repository. A second result for a request changes nothing. For a
// pull repository, kid must be one of the kids of the repository and must have leased the request.
func (m *Manager) Post(ctx context.Context, repo, kid string, p Posted) error {
	if (p.Status != StatusPass && p.Status != StatusFail) || len(p.Summary) > runnerproto.MaxSummary || len(p.LogURL) > runnerproto.MaxLogURL {
		return ErrInvalid
	}
	req, ok, err := m.store.Request(ctx, p.RequestID)
	switch {
	case err != nil:
		return err
	case !ok:
		return ErrUnknownRequest
	case req.Repo != repo:
		return ErrOtherRepo
	}
	if !slices.Contains(m.runners[repo].Kids, kid) {
		return ErrNotRegistered
	}
	if leased, err := m.store.Leased(ctx, req.ID, kid); err != nil {
		return err
	} else if !leased {
		return ErrNotRegistered
	}
	added, err := m.store.AddResult(ctx, Result{RequestID: req.ID, Repo: req.Repo, Tree: req.Tree, Hash: req.Hash, Env: req.Env, Status: p.Status, Summary: p.Summary, LogURL: p.LogURL})
	if err != nil {
		return err
	}
	if added {
		m.broadcast()
	}
	return nil
}

func (m *Manager) spawn(r Request) {
	m.wg.Add(1)
	go m.deliver(m.ctx, r)
}

// deliver wakes the polling runners, waits for the result, and opens a new request when the result is late.
func (m *Manager) deliver(ctx context.Context, req Request) {
	defer m.wg.Done()
	for {
		if m.wake != nil {
			m.wake()
		}
		next, ok := m.awaitResult(ctx, req)
		if !ok {
			return
		}
		req = next
	}
}

// awaitResult waits until the request ends, or until it is late, and then replaces it. An error of the store does not end
// the wait: it is logged and the step is tried again after the backoff.
func (m *Manager) awaitResult(ctx context.Context, req Request) (Request, bool) {
	late := m.clock.After(m.lateIn(req, req.AcceptedAt))
	isLate, failures := false, 0
	for {
		changed := m.Changed()
		cur, ok, err := m.store.Request(ctx, req.ID)
		if err == nil {
			if !ok || !cur.Open {
				return Request{}, false
			}
			if isLate {
				var next Request
				var leased *LeasedError
				if wait := m.lateIn(req, cur.AcceptedAt); wait > 0 {
					late, isLate = m.clock.After(wait), false
				} else if next, ok, err = m.resend(ctx, req); errors.As(err, &leased) {
					late, isLate, err = m.clock.After(max(leased.Until.Sub(m.clock.Now()), time.Second)), false, nil
				} else if err == nil {
					return next, ok
				}
			}
		}
		var retry <-chan time.Time
		if err != nil {
			if ctx.Err() != nil {
				return Request{}, false
			}
			failures++
			m.log.Warn("check request state not read", "request_id", req.ID, "attempt", failures)
			retry = m.clock.After(backoff(failures))
		}
		select {
		case <-changed:
		case <-late:
			isLate, late = true, nil
		case <-retry:
		case <-ctx.Done():
			return Request{}, false
		}
	}
}

// lateIn returns the time until a request that a runner took at from is late. A pull request is taken when a runner leases
// it, so it has no time to take while it waits in the queue.
func (m *Manager) lateIn(req Request, from time.Time) time.Duration {
	limit := time.Duration(req.Check.TimeoutSeconds)*time.Second + resendMargin
	if from.IsZero() {
		return limit
	}
	return from.Add(limit).Sub(m.clock.Now())
}

func (m *Manager) resend(ctx context.Context, old Request) (Request, bool, error) {
	id, err := newID()
	if err != nil {
		return Request{}, false, err
	}
	next := old
	next.ID, next.AcceptedAt, next.Open = id, time.Time{}, true
	ok, err := m.store.Supersede(ctx, old.ID, next, m.clock.Now())
	return next, ok, err
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
