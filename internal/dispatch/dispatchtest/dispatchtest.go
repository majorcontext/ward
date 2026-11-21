// Package dispatchtest is the conformance suite of dispatch.Dispatcher. It plays the manager: it stores each request as open
// and dispatches it, and it plays the runner through the Receive function of the implementation.
package dispatchtest

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/dispatch"
	"github.com/majorcontext/ward/internal/mirror/sqlite"
	"github.com/majorcontext/ward/runnerproto"
)

// Harness is an implementation wired to a runner of each repository that New named.
type Harness struct {
	Dispatcher dispatch.Dispatcher
	// Receive blocks until a runner of repo gets a request and returns it. It returns false when ctx ends first.
	Receive func(ctx context.Context, repo string) (runnerproto.Request, bool)
}

// Impl builds the implementation under test over store, with one runner for each of repos.
type Impl struct {
	New func(t *testing.T, store check.Store, repos []string) Harness
}

const (
	repoA   = "native:alpha"
	repoB   = "native:beta"
	maxWait = 60 * time.Second
)

func request(id, repo, tree string) check.Request {
	c := check.Check{Name: "unit", Run: []string{"make", "test"}, TimeoutSeconds: 60}
	return check.Request{ID: id, Repo: repo, Tree: tree, Commit: strings.Repeat("b", 40), Check: c, Hash: c.Hash(), Env: "env1", Open: true}
}

func wire(r check.Request) runnerproto.Request {
	return runnerproto.Request{RequestID: r.ID, Repo: r.Repo, Tree: r.Tree, Commit: r.Commit, Check: r.Check, CheckHash: r.Hash, Env: r.Env}
}

func send(t *testing.T, store check.Store, d dispatch.Dispatcher, r check.Request) {
	t.Helper()
	if _, created, err := store.OpenRequest(t.Context(), r); err != nil || !created {
		t.Fatalf("open request %s: created %v, %v", r.ID, created, err)
	}
	if err := d.Dispatch(t.Context(), wire(r)); err != nil {
		t.Fatalf("Dispatch %s: %v", r.ID, err)
	}
}

func receive(t *testing.T, h Harness, repo string) runnerproto.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), maxWait)
	defer cancel()
	r, ok := h.Receive(ctx, repo)
	if !ok {
		t.Fatalf("no request reached a runner of %s", repo)
	}
	return r
}

// Run runs the suite against the implementation that impl.New returns.
func Run(t *testing.T, impl Impl) {
	t.Helper()
	fresh := func(t *testing.T) (check.Store, Harness) {
		store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store, impl.New(t, store, []string{repoA, repoB})
	}
	t.Run("delivers the request unchanged", func(t *testing.T) {
		store, h := fresh(t)
		r := request("r1", repoA, strings.Repeat("a", 40))
		send(t, store, h.Dispatcher, r)
		if got := receive(t, h, repoA); !reflect.DeepEqual(got, wire(r)) {
			t.Errorf("got %+v, want %+v", got, wire(r))
		}
	})
	t.Run("delivers each request", func(t *testing.T) {
		store, h := fresh(t)
		send(t, store, h.Dispatcher, request("r1", repoA, strings.Repeat("a", 40)))
		send(t, store, h.Dispatcher, request("r2", repoA, strings.Repeat("c", 40)))
		got := map[string]bool{receive(t, h, repoA).RequestID: true, receive(t, h, repoA).RequestID: true}
		if !got["r1"] || !got["r2"] {
			t.Errorf("got %v, want r1 and r2", got)
		}
	})
	t.Run("delivers to a runner of the repository only", func(t *testing.T) {
		store, h := fresh(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		other := make(chan bool, 1)
		go func() {
			_, ok := h.Receive(ctx, repoB)
			other <- ok
		}()
		send(t, store, h.Dispatcher, request("r1", repoA, strings.Repeat("a", 40)))
		if got := receive(t, h, repoA); got.RequestID != "r1" {
			t.Fatalf("got %+v", got)
		}
		cancel()
		if <-other {
			t.Error("a runner of another repository got the request")
		}
	})
	t.Run("a repository without a runner is refused", func(t *testing.T) {
		_, h := fresh(t)
		if err := h.Dispatcher.Dispatch(t.Context(), wire(request("r1", "native:none", strings.Repeat("a", 40)))); !errors.Is(err, dispatch.ErrNoRunner) {
			t.Errorf("got %v, want ErrNoRunner", err)
		}
	})
}
