// Package warmtest is the conformance suite of warm.Watcher.
package warmtest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/runner/warm"
)

// Clock is a manual check.Clock. Fire ends every wait. A Fire with no wait pending ends the next wait at once.
type Clock struct {
	mu      sync.Mutex
	waiters []chan time.Time
	pending bool
}

func (c *Clock) Now() time.Time { return time.Unix(0, 0) }

func (c *Clock) After(time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if c.pending {
		c.pending = false
		ch <- time.Time{}
		return ch
	}
	c.waiters = append(c.waiters, ch)
	return ch
}

func (c *Clock) Fire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.waiters {
		w <- time.Time{}
	}
	c.pending = len(c.waiters) == 0
	c.waiters = nil
}

// Harness is a watcher over the repository Repo. Change makes the watcher see a change of Repo, and leaves the watcher
// to report it once the clock fires. It may wait until the watcher has started.
type Harness struct {
	Watcher warm.Watcher
	Repo    string
	Change  func()
}

// Impl builds the implementation under test. It must use clock for every wait.
type Impl struct {
	New func(t *testing.T, clock *Clock) Harness
}

const maxWait = 60 * time.Second

// Run checks the contract: a change is reported with its repository or with "", and Watch returns nil when ctx ends.
func Run(t *testing.T, impl Impl) {
	t.Run("reports a change", func(t *testing.T) {
		clock := &Clock{}
		h := impl.New(t, clock)
		ctx, cancel := context.WithTimeout(t.Context(), maxWait)
		defer cancel()
		got := make(chan string, 16)
		done := make(chan error, 1)
		go func() { done <- h.Watcher.Watch(ctx, func(repo string) { got <- repo }) }()
		h.Change()
		clock.Fire()
		select {
		case repo := <-got:
			if repo != h.Repo && repo != "" {
				t.Fatalf("reported %q, want %q or an empty repository", repo, h.Repo)
			}
		case <-ctx.Done():
			t.Fatal("no change was reported")
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Watch returned %v after the context ended, want nil", err)
		}
	})
	t.Run("stops when the context ends", func(t *testing.T) {
		h := impl.New(t, &Clock{})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- h.Watcher.Watch(ctx, func(string) {}) }()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Watch returned %v after the context ended, want nil", err)
		}
	})
}
