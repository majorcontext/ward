package sqlite_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/identity/usertoken"
	"github.com/majorcontext/ward/internal/mirror/sqlite"
	"github.com/majorcontext/ward/internal/usertokentest"
)

func TestOpenRequestOpensNothingForAKeyThatAlreadyHasAPass(t *testing.T) {
	t.Parallel()
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	req := func(id string) check.Request {
		return check.Request{ID: id, Repo: "r", Tree: "t", Hash: "h", Env: "e", Open: true}
	}
	if _, created, err := s.OpenRequest(t.Context(), req("a")); err != nil || !created {
		t.Fatalf("first request: created %v, err %v", created, err)
	}
	if _, err := s.AddResult(t.Context(), check.Result{RequestID: "a", Repo: "r", Tree: "t", Hash: "h", Env: "e", Status: check.StatusPass}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.OpenRequest(t.Context(), req("b")); err != nil || created {
		t.Fatalf("request after a pass: created %v, err %v", created, err)
	}
	if open, err := s.OpenRequests(t.Context()); err != nil || len(open) != 0 {
		t.Fatalf("open requests = %v, err %v", open, err)
	}
}

func TestSupersedeKeepsARequestThatAResultAlreadyEnded(t *testing.T) {
	t.Parallel()
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	req := func(id string) check.Request {
		return check.Request{ID: id, Repo: "r", Tree: "t", Hash: "h", Env: "e", Open: true}
	}
	if _, _, err := s.OpenRequest(t.Context(), req("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResult(t.Context(), check.Result{RequestID: "a", Repo: "r", Tree: "t", Hash: "h", Env: "e", Status: check.StatusFail}); err != nil {
		t.Fatal(err)
	}
	if done, err := s.Supersede(t.Context(), "a", req("b"), time.Unix(1_000_000, 0)); err != nil || done {
		t.Fatalf("supersede of an ended request: done %v, err %v", done, err)
	}
	if open, err := s.OpenRequests(t.Context()); err != nil || len(open) != 0 {
		t.Fatalf("open requests = %v, err %v: a request opened for a key whose request had ended", open, err)
	}
}

func TestUserTokenStoreConformance(t *testing.T) {
	t.Parallel()
	usertokentest.Run(t, func(t *testing.T) usertoken.Store {
		s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
