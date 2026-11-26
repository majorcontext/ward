// Package memory is the in-memory state store of ward. It implements the same store interfaces as the SQLite store and
// runs the same conformance suites. Its data lives as long as the value.
package memory

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/land"
	"github.com/majorcontext/ward/internal/mirror"
)

var (
	_ mirror.Store = (*Store)(nil)
	_ check.Store  = (*Store)(nil)
	_ land.Store   = (*Store)(nil)
)

type refKey struct{ mirror, ref string }

type lease struct {
	id, request, kid string
	until            time.Time
}

// Store keeps all state under one mutex, so each method is atomic as the interfaces require.
type Store struct {
	mu sync.Mutex

	service  mirror.Service
	mirrors  map[string]bool
	seeds    map[string]string
	seedRefs map[refKey]mirror.SeedRef
	cleaned  map[string]bool
	refs     map[refKey]*mirror.Ref

	requests []*check.Request
	results  []check.Result
	leases   []lease

	lands []*land.Land
}

// New returns an empty store.
func New() *Store {
	return &Store{
		service:  mirror.Service{LastSeq: map[int]int64{}},
		mirrors:  map[string]bool{},
		seeds:    map[string]string{},
		seedRefs: map[refKey]mirror.SeedRef{},
		cleaned:  map[string]bool{},
		refs:     map[refKey]*mirror.Ref{},
	}
}

func (s *Store) Close() error { return nil }

func (s *Store) Ping(context.Context) error { return nil }

func (s *Store) lock() func() {
	s.mu.Lock()
	return s.mu.Unlock
}

// clone copies a value through JSON, as the SQLite store stores it, so that the caller and the store never share a slice, a map
// or a pointer.
func clone[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}
