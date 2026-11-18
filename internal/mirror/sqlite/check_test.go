package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/mirror/sqlite"
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
