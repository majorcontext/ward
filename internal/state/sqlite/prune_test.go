package sqlite_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/land"
	"github.com/majorcontext/ward/internal/state/sqlite"
)

// Prune must drop what ended before the cutoff and keep what is open or ended later: dropping an open request loses work.
func TestPruneKeepsOpenRowsAndRowsThatEndedLater(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Unix(1000, 0)
	s, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state.db"), sqlite.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	req := func(id, tree string) check.Request {
		return check.Request{ID: id, Repo: "r", Tree: tree, Hash: "h", Env: "e", Open: true}
	}
	for _, id := range []string{"old", "open", "late"} {
		if _, _, err := s.OpenRequest(ctx, req(id, id)); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertLand(ctx, land.Land{ID: id, Repo: "r", Ref: "main", State: land.Queued}); err != nil {
			t.Fatal(err)
		}
	}
	end := func(id string, at time.Time) {
		now = at
		if _, err := s.AddResult(ctx, check.Result{RequestID: id, Repo: "r", Tree: id, Hash: "h", Env: "e", Status: check.StatusFail}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveLand(ctx, land.Land{ID: id, Repo: "r", Ref: "main", State: land.Failed}); err != nil {
			t.Fatal(err)
		}
	}
	end("old", time.Unix(10, 0))
	end("late", time.Unix(2000, 0))
	if _, _, err := s.Lease(ctx, []string{"r"}, "k", "lease", time.Unix(5, 0), time.Unix(6, 0)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Prune(ctx, time.Unix(1500, 0)); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"old": false, "open": true, "late": true} {
		if _, ok, err := s.Request(ctx, id); err != nil || ok != want {
			t.Errorf("request %s present = %v (%v), want %v", id, ok, err, want)
		}
		if _, ok, err := s.Land(ctx, id); err != nil || ok != want {
			t.Errorf("land %s present = %v (%v), want %v", id, ok, err, want)
		}
	}
}
