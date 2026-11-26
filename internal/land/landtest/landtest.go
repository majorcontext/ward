// Package landtest is the conformance suite of land.Store.
package landtest

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/majorcontext/ward/internal/land"
)

func ids(ls []land.Land) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.ID)
	}
	return out
}

// Run runs the suite on a new Store from newStore for each case.
func Run(t *testing.T, newStore func(t *testing.T) land.Store) {
	ctx := context.Background()
	mk := func(id, repo, ref string) land.Land {
		return land.Land{ID: id, Repo: repo, Ref: ref, Branch: "b", State: land.Queued, Checks: []land.CheckState{{Name: "static"}}}
	}
	t.Run("a land is stored whole and found by ID", func(t *testing.T) {
		s := newStore(t)
		in := mk("l1", "r", "main")
		in.Error = &land.Error{Code: land.CodeStale, Message: "m"}
		in.Call = &land.Call{Base: "x", Head: "y", BranchGen: 3}
		if err := s.InsertLand(ctx, in); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.Land(ctx, "l1")
		if err != nil || !ok || !reflect.DeepEqual(got, in) {
			t.Fatalf("land = %+v, %v, %v; want %+v", got, ok, err, in)
		}
		got.Checks[0].State = "pass"
		got.Error.Details = map[string]any{"x": 1}
		in.Checks[0].Name = "changed"
		if again, _, _ := s.Land(ctx, "l1"); again.Checks[0].State != "" || again.Checks[0].Name != "static" || again.Error.Details != nil {
			t.Fatalf("a caller changed the stored land: %+v", again)
		}
		in.Checks[0].Name = "static"
		if _, ok, err := s.Land(ctx, "nope"); err != nil || ok {
			t.Fatalf("unknown land: %v, %v", ok, err)
		}
		if err := s.InsertLand(ctx, in); err == nil {
			t.Fatal("a second insert of one ID succeeded")
		}
	})
	t.Run("the queue of a key keeps insertion order and drops ended lands", func(t *testing.T) {
		s := newStore(t)
		for _, l := range []land.Land{mk("a", "r", "main"), mk("b", "r", "main"), mk("c", "r", "dev"), mk("d", "r", "main")} {
			if err := s.InsertLand(ctx, l); err != nil {
				t.Fatal(err)
			}
		}
		next := func(n int) []string {
			ls, err := s.NextLands(ctx, land.Key{Repo: "r", Ref: "main"}, n)
			if err != nil {
				t.Fatal(err)
			}
			return ids(ls)
		}
		if got := next(10); !slices.Equal(got, []string{"a", "b", "d"}) {
			t.Fatalf("queue = %v", got)
		}
		if got := next(2); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("limited queue = %v", got)
		}
		a, _, _ := s.Land(ctx, "a")
		a.State = land.Landed
		if err := s.SaveLand(ctx, a); err != nil {
			t.Fatal(err)
		}
		if got := next(10); !slices.Equal(got, []string{"b", "d"}) {
			t.Fatalf("queue after a landed = %v", got)
		}
		b, _, _ := s.Land(ctx, "b")
		b.State, b.Attempt = land.Checking, 2
		if err := s.SaveLand(ctx, b); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := s.Land(ctx, "b"); got.Attempt != 2 || got.State != land.Checking {
			t.Fatalf("saved land = %+v", got)
		}
	})
	t.Run("unfinished keys list each key with a land that has not ended", func(t *testing.T) {
		s := newStore(t)
		for _, l := range []land.Land{mk("a", "r", "main"), mk("b", "r", "main"), mk("c", "r", "dev")} {
			if err := s.InsertLand(ctx, l); err != nil {
				t.Fatal(err)
			}
		}
		c, _, _ := s.Land(ctx, "c")
		c.State = land.Failed
		if err := s.SaveLand(ctx, c); err != nil {
			t.Fatal(err)
		}
		got, err := s.UnfinishedKeys(ctx)
		if err != nil || !reflect.DeepEqual(got, []land.Key{{Repo: "r", Ref: "main"}}) {
			t.Fatalf("keys = %v, %v", got, err)
		}
	})
}
