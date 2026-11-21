// Package usertokentest holds the conformance suite of usertoken.Store and an in-memory Store that runs it.
package usertokentest

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/identity/usertoken"
)

var at = time.Unix(2_000_000, 0).UTC()

// Run runs the suite on a new Store from newStore for each case.
func Run(t *testing.T, newStore func(t *testing.T) usertoken.Store) {
	t.Helper()
	ctx := context.Background()
	rec := func(id string, created time.Time) usertoken.Record {
		return usertoken.Record{ID: id, Principal: "dev@example.test", Scope: usertoken.ScopeLand, Repos: []string{"native:repo_A", "native:repo_B"}, CreatedAt: created}
	}
	t.Run("a token is stored by hash and never twice", func(t *testing.T) {
		s := newStore(t)
		want := rec("ut_1", at)
		want.ExpiresAt = at.Add(time.Hour)
		if err := s.CreateUserToken(ctx, "h1", want); err != nil {
			t.Fatal(err)
		}
		got, err := s.UserTokenByHash(ctx, "h1")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("read = %+v, %v, want %+v", got, err, want)
		}
		for _, c := range []struct{ hash, id string }{{"h1", "ut_2"}, {"h2", "ut_1"}} {
			if err := s.CreateUserToken(ctx, c.hash, rec(c.id, at)); !errors.Is(err, usertoken.ErrExists) {
				t.Errorf("create %v: err = %v, want ErrExists", c, err)
			}
		}
		if _, err := s.UserTokenByHash(ctx, "missing"); !errors.Is(err, usertoken.ErrNotFound) {
			t.Errorf("read of an unknown hash: err = %v, want ErrNotFound", err)
		}
	})
	t.Run("revoke keeps the first time and refuses an unknown id", func(t *testing.T) {
		s := newStore(t)
		_ = s.CreateUserToken(ctx, "h", rec("ut_1", at))
		if err := errors.Join(s.RevokeUserToken(ctx, "ut_1", at.Add(time.Minute)), s.RevokeUserToken(ctx, "ut_1", at.Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
		got, _ := s.UserTokenByHash(ctx, "h")
		if !got.RevokedAt.Equal(at.Add(time.Minute)) {
			t.Errorf("revoked at %v, want the first time", got.RevokedAt)
		}
		if err := s.RevokeUserToken(ctx, "ut_none", at); !errors.Is(err, usertoken.ErrNotFound) {
			t.Errorf("revoke of an unknown id: err = %v, want ErrNotFound", err)
		}
	})
	t.Run("tokens list newest first with ties by id descending", func(t *testing.T) {
		s := newStore(t)
		for i, id := range []string{"ut_a", "ut_b", "ut_c", "ut_d"} {
			_ = s.CreateUserToken(ctx, "h-"+id, rec(id, at.Add(time.Duration(i/2)*time.Second)))
		}
		got, err := s.UserTokens(ctx)
		var ids []string
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		if want := []string{"ut_d", "ut_c", "ut_b", "ut_a"}; err != nil || !slices.Equal(ids, want) {
			t.Errorf("order = %v, %v, want %v", ids, err, want)
		}
	})
}

// Mem is a Store in memory.
type Mem struct {
	mu   sync.Mutex
	byID map[string]usertoken.Record
	hash map[string]string
}

// NewMem returns an empty Mem.
func NewMem() *Mem { return &Mem{byID: map[string]usertoken.Record{}, hash: map[string]string{}} }

func (m *Mem) CreateUserToken(_ context.Context, hash string, r usertoken.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[r.ID]; ok || m.hash[hash] != "" {
		return usertoken.ErrExists
	}
	m.byID[r.ID], m.hash[hash] = r, r.ID
	return nil
}

func (m *Mem) UserTokenByHash(_ context.Context, hash string) (usertoken.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[m.hash[hash]]
	if !ok {
		return usertoken.Record{}, usertoken.ErrNotFound
	}
	return r, nil
}

func (m *Mem) UserTokens(context.Context) ([]usertoken.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]usertoken.Record, 0, len(m.byID))
	for _, r := range m.byID {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b usertoken.Record) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	return out, nil
}

func (m *Mem) RevokeUserToken(_ context.Context, id string, when time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok {
		return usertoken.ErrNotFound
	}
	if r.RevokedAt.IsZero() {
		r.RevokedAt = when
		m.byID[id] = r
	}
	return nil
}
