// Package checktest is the conformance suite of check.Store.
package checktest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/check"
)

var at = time.Unix(2_000_000, 0).UTC()

func req(id, tree string) check.Request {
	return check.Request{ID: id, Repo: "r", Tree: tree, Hash: "h", Env: "e"}
}

func res(id, tree string, st check.Status) check.Result {
	return check.Result{RequestID: id, Repo: "r", Tree: tree, Hash: "h", Env: "e", Status: st}
}

func openIDs(t *testing.T, s check.Store) []string {
	t.Helper()
	open, err := s.OpenRequests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range open {
		ids = append(ids, r.ID)
	}
	return ids
}

func want(t *testing.T, got []string, ids ...string) {
	t.Helper()
	if len(got) != len(ids) {
		t.Fatalf("got %v, want %v", got, ids)
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("got %v, want %v", got, ids)
		}
	}
}

// Run runs the suite on a new Store from newStore for each case.
func Run(t *testing.T, newStore func(t *testing.T) check.Store) {
	ctx := context.Background()
	t.Run("one open request for a key, and none after a pass", func(t *testing.T) {
		s := newStore(t)
		if _, created, err := s.OpenRequest(ctx, req("a", "t")); err != nil || !created {
			t.Fatalf("first: created %v, err %v", created, err)
		}
		if got, created, err := s.OpenRequest(ctx, req("b", "t")); err != nil || created || got.ID != "a" {
			t.Fatalf("second: %+v created %v, err %v; want the open request a", got, created, err)
		}
		if _, err := s.AddResult(ctx, res("a", "t", check.StatusPass)); err != nil {
			t.Fatal(err)
		}
		if _, created, err := s.OpenRequest(ctx, req("c", "t")); err != nil || created {
			t.Fatalf("after a pass: created %v, err %v", created, err)
		}
		want(t, openIDs(t, s))
	})
	t.Run("a result closes its request once and orders by ID", func(t *testing.T) {
		s := newStore(t)
		for _, id := range []string{"a", "b"} {
			if _, _, err := s.OpenRequest(ctx, req(id, id)); err != nil {
				t.Fatal(err)
			}
		}
		if added, err := s.AddResult(ctx, res("a", "a", check.StatusFail)); err != nil || !added {
			t.Fatalf("first result: %v, %v", added, err)
		}
		if added, err := s.AddResult(ctx, res("a", "a", check.StatusPass)); err != nil || added {
			t.Fatalf("second result for one request: %v, %v", added, err)
		}
		want(t, openIDs(t, s), "b")
		if _, err := s.AddResult(ctx, res("b", "b", check.StatusFail)); err != nil {
			t.Fatal(err)
		}
		if n, err := s.MaxResultID(ctx); err != nil || n < 2 {
			t.Fatalf("max id = %d, %v", n, err)
		}
		got, err := s.Results(ctx, "r", "a", "h", "e")
		if err != nil || len(got) != 1 || got[0].RequestID != "a" || got[0].Status != check.StatusFail {
			t.Fatalf("results = %+v, %v", got, err)
		}
	})
	t.Run("a pass closes the other open requests of its key", func(t *testing.T) {
		s := newStore(t)
		if _, _, err := s.OpenRequest(ctx, req("a", "t")); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Supersede(ctx, "a", req("b", "t"), at); err != nil || !ok {
			t.Fatalf("supersede: %v, %v", ok, err)
		}
		if _, ok, err := s.Lease(ctx, []string{"r"}, "k", "l1", at, at.Add(time.Hour)); err != nil || !ok {
			t.Fatalf("lease: %v, %v", ok, err)
		}
		want(t, openIDs(t, s), "b")
		if _, err := s.AddResult(ctx, res("a", "t", check.StatusPass)); err != nil {
			t.Fatal(err)
		}
		want(t, openIDs(t, s))
		if ok, _ := s.RenewLease(ctx, "l1", "k", []string{"r"}, at.Add(2*time.Hour)); ok {
			t.Error("a lease of a request that a pass closed renewed")
		}
	})
	t.Run("a request that the caller changes after a read stays as stored", func(t *testing.T) {
		s := newStore(t)
		in := req("a", "t")
		in.Check.Run = []string{"go", "test"}
		if _, _, err := s.OpenRequest(ctx, in); err != nil {
			t.Fatal(err)
		}
		in.Check.Run[0] = "x"
		got, _, _ := s.Request(ctx, "a")
		got.Check.Run[1] = "y"
		if again, _, _ := s.Request(ctx, "a"); again.Check.Run[0] != "go" || again.Check.Run[1] != "test" {
			t.Fatalf("stored run = %v", again.Check.Run)
		}
	})
	t.Run("supersede keeps a request that a result ended", func(t *testing.T) {
		s := newStore(t)
		if _, _, err := s.OpenRequest(ctx, req("a", "t")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddResult(ctx, res("a", "t", check.StatusFail)); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Supersede(ctx, "a", req("b", "t"), at); err != nil || ok {
			t.Fatalf("supersede after a pass: %v, %v", ok, err)
		}
		want(t, openIDs(t, s))
	})
	t.Run("a live lease blocks supersede and a dead lease does not", func(t *testing.T) {
		s := newStore(t)
		if _, _, err := s.OpenRequest(ctx, req("a", "t")); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.Lease(ctx, []string{"r"}, "k", "l1", at, at.Add(time.Minute)); err != nil || !ok {
			t.Fatalf("lease: %v, %v", ok, err)
		}
		var le *check.LeasedError
		if ok, err := s.Supersede(ctx, "a", req("b", "t"), at.Add(time.Second)); ok || !errors.As(err, &le) || !le.Until.Equal(at.Add(time.Minute)) {
			t.Fatalf("supersede under a live lease: %v, %v", ok, err)
		}
		if ok, err := s.Supersede(ctx, "a", req("b", "t"), at.Add(time.Hour)); err != nil || !ok {
			t.Fatalf("supersede after the lease ended: %v, %v", ok, err)
		}
	})
	t.Run("a lease gives the oldest free request of the repositories and a dead lease frees it", func(t *testing.T) {
		s := newStore(t)
		other := req("x", "x")
		other.Repo = "other"
		for _, r := range []check.Request{other, req("a", "a"), req("b", "b")} {
			if _, _, err := s.OpenRequest(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok, err := s.Lease(ctx, nil, "k", "l0", at, at.Add(time.Minute)); err != nil || ok {
			t.Fatalf("lease with no repositories: %v, %v", ok, err)
		}
		got, ok, err := s.Lease(ctx, []string{"r"}, "k", "l1", at, at.Add(time.Minute))
		if err != nil || !ok || got.ID != "a" || !got.AcceptedAt.Equal(at) {
			t.Fatalf("first lease: %+v, %v, %v", got, ok, err)
		}
		if got, _, _ = s.Lease(ctx, []string{"r"}, "k2", "l2", at, at.Add(time.Minute)); got.ID != "b" {
			t.Fatalf("second lease = %q, want b", got.ID)
		}
		if _, ok, _ = s.Lease(ctx, []string{"r"}, "k3", "l3", at, at.Add(time.Minute)); ok {
			t.Fatal("a third lease found a request with a live lease")
		}
		if got, ok, _ = s.Lease(ctx, []string{"r"}, "k3", "l3", at.Add(time.Hour), at.Add(2*time.Hour)); !ok || got.ID != "a" {
			t.Fatalf("lease after expiry = %+v, %v; want a again", got, ok)
		}
	})
	t.Run("only the newest lease of the holder renews, on an open request of its repositories", func(t *testing.T) {
		s := newStore(t)
		if _, _, err := s.OpenRequest(ctx, req("a", "t")); err != nil {
			t.Fatal(err)
		}
		s.Lease(ctx, []string{"r"}, "k", "l1", at, at.Add(time.Minute))
		for _, c := range []struct {
			lease, kid string
			repos      []string
			ok         bool
		}{{"l1", "k", []string{"r"}, true}, {"l1", "other", []string{"r"}, false}, {"l1", "k", []string{"x"}, false}, {"l1", "k", nil, false}, {"nope", "k", []string{"r"}, false}} {
			if ok, err := s.RenewLease(ctx, c.lease, c.kid, c.repos, at.Add(time.Hour)); err != nil || ok != c.ok {
				t.Errorf("renew %+v = %v, %v", c, ok, err)
			}
		}
		s.Lease(ctx, []string{"r"}, "k2", "l2", at.Add(2*time.Hour), at.Add(3*time.Hour))
		if ok, _ := s.RenewLease(ctx, "l1", "k", []string{"r"}, at.Add(4*time.Hour)); ok {
			t.Error("a replaced lease renewed")
		}
		if held, err := s.Leased(ctx, "a", "k"); err != nil || !held {
			t.Errorf("leased k = %v, %v", held, err)
		}
		if held, _ := s.Leased(ctx, "a", "zz"); held {
			t.Error("leased zz")
		}
		s.AddResult(ctx, res("a", "t", check.StatusFail))
		if ok, _ := s.RenewLease(ctx, "l2", "k2", []string{"r"}, at.Add(5*time.Hour)); ok {
			t.Error("a lease of a closed request renewed")
		}
	})
}
