package pull_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/dispatch/dispatchtest"
	"github.com/majorcontext/ward/internal/dispatch/pull"
	"github.com/majorcontext/ward/internal/mirror/sqlite"
	"github.com/majorcontext/ward/runnerproto"
)

func TestConformance(t *testing.T) {
	dispatchtest.Run(t, dispatchtest.Impl{New: func(_ *testing.T, store check.Store, repos []string) dispatchtest.Harness {
		var runners []check.Runner
		for _, repo := range repos {
			runners = append(runners, check.Runner{Repo: repo, Mode: check.ModePull, Kids: []string{repo + "-runner"}, MinIsolation: string(runnerproto.IsolationGVisor), Env: "env1"})
		}
		q := pull.New(store, runners, nil)
		return dispatchtest.Harness{Dispatcher: q, Receive: func(ctx context.Context, repo string) (runnerproto.Request, bool) {
			l, ok, err := q.Lease(ctx, repo+"-runner", runnerproto.IsolationGVisor, time.Hour)
			return l.Request, ok && err == nil
		}}
	}})
}

func newQueue(t *testing.T) (*pull.Queue, check.Store) {
	t.Helper()
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runners := []check.Runner{
		{Repo: "r", Mode: check.ModePull, Kids: []string{"strong", "weak"}, MinIsolation: string(runnerproto.IsolationGVisor)},
		{Repo: "lax", Mode: check.ModePull, Kids: []string{"weak"}, MinIsolation: string(runnerproto.IsolationContainer)},
	}
	return pull.New(store, runners, nil), store
}

func open(t *testing.T, store check.Store, repo string) {
	t.Helper()
	c := check.Check{Name: "unit", Run: []string{"x"}, TimeoutSeconds: 60}
	if _, _, err := store.OpenRequest(t.Context(), check.Request{ID: "req-" + repo, Repo: repo, Tree: "t", Commit: "c", Check: c, Hash: c.Hash(), Open: true}); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredLeaseReturnsTheRequestToTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q, store := newQueue(t)
		open(t, store, "r")
		first, ok, err := q.Lease(t.Context(), "strong", runnerproto.IsolationGVisor, time.Second)
		if err != nil || !ok {
			t.Fatalf("first lease: %v %v", ok, err)
		}
		if _, ok, _ := q.Lease(t.Context(), "strong", runnerproto.IsolationGVisor, time.Second); ok {
			t.Fatal("a request with a live lease was leased again")
		}
		time.Sleep(pull.LeaseTTL / 2)
		if _, err := q.Renew(t.Context(), "strong", first.LeaseID); err != nil {
			t.Fatalf("renew: %v", err)
		}
		time.Sleep(pull.LeaseTTL/2 + time.Second)
		if _, ok, _ := q.Lease(t.Context(), "strong", runnerproto.IsolationGVisor, time.Second); ok {
			t.Fatal("a renewed lease ended at its first time")
		}
		time.Sleep(pull.LeaseTTL)
		if _, ok, err := q.Lease(t.Context(), "weak", runnerproto.IsolationContainer, time.Second); err != nil || ok {
			t.Fatalf("a weak runner leased a gvisor repository: %v %v", ok, err)
		}
		second, ok, err := q.Lease(t.Context(), "strong", runnerproto.IsolationGVisor, time.Second)
		if err != nil || !ok || second.Request.RequestID != first.Request.RequestID || second.LeaseID == first.LeaseID {
			t.Fatalf("second lease = %+v %v %v, want the same request under a new lease", second, ok, err)
		}
		if _, err := q.Renew(t.Context(), "strong", first.LeaseID); !errors.Is(err, pull.ErrLeaseLost) {
			t.Errorf("renew of a replaced lease: %v", err)
		}
		if _, err := q.Renew(t.Context(), "weak", second.LeaseID); !errors.Is(err, pull.ErrLeaseLost) {
			t.Errorf("renew by another runner: %v", err)
		}
	})
}

func TestLeaseRefusesAnUnknownRunnerAndAWeakSandbox(t *testing.T) {
	q, store := newQueue(t)
	open(t, store, "r")
	for _, c := range []struct {
		kid  string
		iso  runnerproto.Isolation
		want error
	}{
		{"stranger", runnerproto.IsolationGVisor, pull.ErrUnknownRunner},
		{"strong", runnerproto.IsolationContainer, pull.ErrWeakIsolation},
		{"strong", "none", pull.ErrWeakIsolation},
	} {
		if _, _, err := q.Lease(t.Context(), c.kid, c.iso, time.Second); !errors.Is(err, c.want) {
			t.Errorf("%s with %s: %v, want %v", c.kid, c.iso, err, c.want)
		}
	}
	if _, ok, err := q.Lease(t.Context(), "weak", runnerproto.IsolationContainer, time.Second); err != nil || ok {
		t.Errorf("weak runner took the request of a gvisor repository: %v %v", ok, err)
	}
	open(t, store, "lax")
	if l, ok, err := q.Lease(t.Context(), "weak", runnerproto.IsolationContainer, time.Second); err != nil || !ok || l.Request.Repo != "lax" {
		t.Errorf("weak runner on the repository that allows it: %+v %v %v", l, ok, err)
	}
}
