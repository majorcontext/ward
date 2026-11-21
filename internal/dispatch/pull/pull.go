// Package pull dispatches a check request through a queue that runners poll. A runner dials out to ward, holds a lease on a
// request while it runs it, and renews the lease. A lease that is not renewed ends, and the same request is leasable again.
package pull

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/dispatch"
	"github.com/majorcontext/ward/runnerproto"
)

const (
	// LeaseTTL is how long a lease lasts without a renewal.
	LeaseTTL = 60 * time.Second
	// recheck is how often a waiting poll looks for a lease that ended.
	recheck = 5 * time.Second
)

var (
	// ErrUnknownRunner means that the kid is registered for no pull repository.
	ErrUnknownRunner = errors.New("pull: runner not registered")
	// ErrWeakIsolation means that the sandbox of the runner is weaker than every repository that it serves allows.
	ErrWeakIsolation = errors.New("pull: sandbox weaker than the minimum")
	// ErrLeaseLost means that the lease ended, was replaced, or its request is closed.
	ErrLeaseLost = errors.New("pull: lease lost")
)

// Queue is the queue of the pull repositories.
type Queue struct {
	store   check.Store
	clock   check.Clock
	runners map[string]check.Runner

	mu   sync.Mutex
	wake chan struct{}
}

var _ dispatch.Dispatcher = (*Queue)(nil)

// New returns a queue for the pull runners in runners. A nil clock means the wall clock.
func New(store check.Store, runners []check.Runner, clock check.Clock) *Queue {
	if clock == nil {
		clock = check.RealClock()
	}
	q := &Queue{store: store, clock: clock, runners: map[string]check.Runner{}, wake: make(chan struct{})}
	for _, r := range runners {
		if r.Pull() {
			q.runners[r.Repo] = r
		}
	}
	return q
}

// Dispatch queues req. The store holds the request already, so the queue only wakes the polls.
func (q *Queue) Dispatch(_ context.Context, req runnerproto.Request) error {
	if _, ok := q.runners[req.Repo]; !ok {
		return dispatch.ErrNoRunner
	}
	q.mu.Lock()
	close(q.wake)
	q.wake = make(chan struct{})
	q.mu.Unlock()
	return nil
}

func (q *Queue) woken() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.wake
}

// repos returns the repositories that kid may serve with a sandbox of level iso.
func (q *Queue) repos(kid string, iso runnerproto.Isolation) ([]string, error) {
	var out []string
	known := false
	for repo, r := range q.runners {
		for _, k := range r.Kids {
			if k != kid {
				continue
			}
			known = true
			if iso.AtLeast(runnerproto.Isolation(r.MinIsolation)) {
				out = append(out, repo)
			}
		}
	}
	switch {
	case !known:
		return nil, ErrUnknownRunner
	case len(out) == 0:
		return nil, ErrWeakIsolation
	}
	return out, nil
}

// Lease waits up to wait for a request of a repository that kid serves, and leases it. It returns false when none came.
func (q *Queue) Lease(ctx context.Context, kid string, iso runnerproto.Isolation, wait time.Duration) (runnerproto.Lease, bool, error) {
	repos, err := q.repos(kid, iso)
	if err != nil {
		return runnerproto.Lease{}, false, err
	}
	deadline := q.clock.After(wait)
	for {
		wake := q.woken()
		id, err := newID()
		if err != nil {
			return runnerproto.Lease{}, false, err
		}
		now := q.clock.Now()
		req, ok, err := q.store.Lease(ctx, repos, kid, id, now, now.Add(LeaseTTL))
		if err != nil {
			return runnerproto.Lease{}, false, err
		}
		if ok {
			return runnerproto.Lease{LeaseID: id, TTLSeconds: int(LeaseTTL / time.Second), Request: runnerproto.Request{
				RequestID: req.ID, Repo: req.Repo, Tree: req.Tree, Commit: req.Commit, Check: req.Check, CheckHash: req.Hash, Env: req.Env}}, true, nil
		}
		select {
		case <-wake:
		case <-q.clock.After(recheck):
		case <-deadline:
			return runnerproto.Lease{}, false, nil
		case <-ctx.Done():
			return runnerproto.Lease{}, false, ctx.Err()
		}
	}
}

// Renew extends the lease leaseID of kid by LeaseTTL from now. It refuses a kid that no pull repository registers any more.
func (q *Queue) Renew(ctx context.Context, kid, leaseID string) (runnerproto.Renewal, error) {
	var repos []string
	for repo, r := range q.runners {
		if slices.Contains(r.Kids, kid) {
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		return runnerproto.Renewal{}, ErrUnknownRunner
	}
	ok, err := q.store.RenewLease(ctx, leaseID, kid, repos, q.clock.Now().Add(LeaseTTL))
	switch {
	case err != nil:
		return runnerproto.Renewal{}, err
	case !ok:
		return runnerproto.Renewal{}, ErrLeaseLost
	}
	return runnerproto.Renewal{TTLSeconds: int(LeaseTTL / time.Second)}, nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
