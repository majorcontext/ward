package check_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/dispatch"
	"github.com/majorcontext/ward/internal/dispatch/push"
	"github.com/majorcontext/ward/internal/mirror/sqlite"
)

// stepClock is a clock that a test moves.
type stepClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []stepTimer
	added  chan struct{}
}

type stepTimer struct {
	at time.Time
	ch chan time.Time
}

func (c *stepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.timers = append(c.timers, stepTimer{c.now.Add(d), ch})
	c.mu.Unlock()
	c.added <- struct{}{}
	return ch
}

// waitTimer returns when a timer of d is set.
func (c *stepClock) waitTimer(t *testing.T, d time.Duration) {
	t.Helper()
	for {
		c.mu.Lock()
		for _, tm := range c.timers {
			if tm.at.Equal(c.now.Add(d)) {
				c.mu.Unlock()
				return
			}
		}
		c.mu.Unlock()
		select {
		case <-c.added:
		case <-time.After(20 * time.Second): //lint:allow wallclock a wait on a goroutine that no bubble holds
			t.Fatalf("no timer of %s", d)
		}
	}
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	rest := c.timers[:0]
	for _, tm := range c.timers {
		if c.now.Before(tm.at) {
			rest = append(rest, tm)
		} else {
			tm.ch <- c.now
		}
	}
	c.timers = rest
}

// readFailer makes the next reads of a request fail.
type readFailer struct {
	check.Store
	fail atomic.Int32
}

func (f *readFailer) Request(ctx context.Context, id string) (check.Request, bool, error) {
	if f.fail.Add(-1) >= 0 {
		return check.Request{}, false, errors.New("the store is busy")
	}
	return f.Store.Request(ctx, id)
}

func TestLateRequestIsSentAgainAfterAStoreReadFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st, err := sqlite.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	store := &readFailer{Store: st}
	got := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { got <- struct{}{} }))
	defer srv.Close()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("a-secret-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := &stepClock{now: time.Unix(1_000_000, 0), added: make(chan struct{}, 64)}
	runners := []check.Runner{{Repo: "r", URL: srv.URL, SecretFile: secret, Env: "e"}}
	m := check.New(store, runners, map[string]dispatch.Dispatcher{"r": push.New(runners)}, clk, nil)
	ctx, cancel := context.WithCancel(t.Context())
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); m.Wait() }()
	recv := func(what string) {
		t.Helper()
		select {
		case <-got:
		case <-time.After(20 * time.Second): //lint:allow wallclock a wait on a goroutine that no bubble holds
			t.Fatalf("the runner got no %s", what)
		}
	}

	if err := m.Ensure(ctx, "r", "tree", "commit", check.Check{Name: "unit", Run: []string{"x"}, TimeoutSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	recv("first request")
	late := 60*time.Second + 10*time.Minute
	clk.waitTimer(t, late)
	store.fail.Store(1)
	clk.advance(late)
	clk.waitTimer(t, time.Second)
	clk.advance(time.Second)
	recv("request after the read failed")
}
