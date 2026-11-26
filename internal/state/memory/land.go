package memory

import (
	"context"
	"errors"

	"github.com/majorcontext/ward/internal/land"
)

func (s *Store) InsertLand(_ context.Context, l land.Land) error {
	defer s.lock()()
	for _, have := range s.lands {
		if have.ID == l.ID {
			return errors.New("memory: duplicate land id")
		}
	}
	c := clone(l)
	s.lands = append(s.lands, &c)
	return nil
}

func (s *Store) Land(_ context.Context, id string) (land.Land, bool, error) {
	defer s.lock()()
	for _, l := range s.lands {
		if l.ID == id {
			return clone(*l), true, nil
		}
	}
	return land.Land{}, false, nil
}

func (s *Store) NextLands(_ context.Context, key land.Key, n int) ([]land.Land, error) {
	defer s.lock()()
	var out []land.Land
	for _, l := range s.lands {
		if len(out) < n && l.Repo == key.Repo && l.Ref == key.Ref && !l.State.Ended() {
			out = append(out, clone(*l))
		}
	}
	return out, nil
}

func (s *Store) SaveLand(_ context.Context, l land.Land) error {
	defer s.lock()()
	for _, have := range s.lands {
		if have.ID == l.ID {
			*have = clone(l)
		}
	}
	return nil
}

func (s *Store) UnfinishedKeys(context.Context) ([]land.Key, error) {
	defer s.lock()()
	var out []land.Key
	seen := map[land.Key]bool{}
	for _, l := range s.lands {
		k := land.Key{Repo: l.Repo, Ref: l.Ref}
		if !l.State.Ended() && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, nil
}
