package memory

import (
	"context"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/majorcontext/motte/api"

	"github.com/majorcontext/ward/internal/mirror"
)

func (s *Store) Service(context.Context) (mirror.Service, error) {
	defer s.lock()()
	out := s.service
	out.LastSeq = maps.Clone(s.service.LastSeq)
	return out, nil
}

func (s *Store) SetSubscription(_ context.Context, id, cursor string, clearSeq bool) error {
	defer s.lock()()
	s.service.SubscriptionID, s.service.EventsCursor = id, cursor
	if clearSeq {
		clear(s.service.LastSeq)
	}
	return nil
}

func (s *Store) advance(e api.Event) mirror.HookResult {
	last, ok := s.service.LastSeq[e.Partition]
	res := mirror.HookInOrder
	switch {
	case !ok:
		res = mirror.HookBaseline
	case e.Seq <= last:
		return mirror.HookRepeat
	case e.Seq > last+1:
		res = mirror.HookGap
	}
	s.service.LastSeq[e.Partition] = e.Seq
	return res
}

func (s *Store) trigger(name, ref string, partition int, seq int64, now time.Time) {
	k := refKey{name, ref}
	r := s.refs[k]
	if r == nil {
		r = &mirror.Ref{Mirror: name, Name: ref, LastPartition: -1}
		s.refs[k] = r
	}
	if !r.Dirty {
		r.DirtySince = now
	}
	r.Dirty = true
	r.Version++
	r.NextAttempt = now
	if partition >= 0 && (r.LastPartition != partition || seq > r.LastSeq) {
		r.LastPartition, r.LastSeq = partition, seq
	}
}

func (s *Store) RecordHook(_ context.Context, hit mirror.Hit, now time.Time) (mirror.HookResult, error) {
	defer s.lock()()
	res := s.advance(hit.Event)
	if res == mirror.HookRepeat {
		return res, nil
	}
	for _, m := range hit.Mirrors {
		s.trigger(m, hit.Event.Ref, hit.Event.Partition, hit.Event.Seq, now)
	}
	return res, nil
}

func (s *Store) RecordPage(_ context.Context, hits []mirror.Hit, cursor string, now time.Time) error {
	defer s.lock()()
	for _, h := range hits {
		e := h.Event
		if last, ok := s.service.LastSeq[e.Partition]; !ok || e.Seq > last {
			s.service.LastSeq[e.Partition] = e.Seq
		}
		for _, m := range h.Mirrors {
			s.trigger(m, e.Ref, e.Partition, e.Seq, now)
		}
	}
	s.service.EventsCursor = cursor
	return nil
}

func (s *Store) sortedRefs(name string) []mirror.Ref {
	var out []mirror.Ref
	for k, r := range s.refs {
		if k.mirror == name {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) Due(_ context.Context, name string, now time.Time) ([]mirror.Ref, time.Time, error) {
	defer s.lock()()
	var due []mirror.Ref
	var next time.Time
	for _, r := range s.sortedRefs(name) {
		switch {
		case !r.Dirty:
		case !r.NextAttempt.After(now):
			due = append(due, r)
		case next.IsZero() || r.NextAttempt.Before(next):
			next = r.NextAttempt
		}
	}
	return due, next, nil
}

func (s *Store) RecordPush(_ context.Context, name, ref string, version int64, pushed string, now time.Time) error {
	defer s.lock()()
	if r := s.refs[refKey{name, ref}]; r != nil {
		r.Pushed, r.PushedSet, r.PushedAt = pushed, true, now
		r.Failure, r.Message, r.FailureSince, r.Attempts = "", "", time.Time{}, 0
		if r.Version == version {
			r.Dirty = false
		}
	}
	return nil
}

func (s *Store) RecordFailure(_ context.Context, name, ref string, version int64, class mirror.Class, message string, next, now time.Time) error {
	defer s.lock()()
	if r := s.refs[refKey{name, ref}]; r != nil {
		if r.Failure == "" {
			r.FailureSince = now
		}
		r.Failure, r.Message = class, message
		r.Attempts++
		if r.Version == version {
			r.NextAttempt = next
		}
	}
	return nil
}

func (s *Store) SetPushed(_ context.Context, name, ref, pushed string) error {
	defer s.lock()()
	if r := s.refs[refKey{name, ref}]; r != nil {
		r.Pushed, r.PushedSet = pushed, true
	}
	return nil
}

func (s *Store) Reconciled(_ context.Context, name string, targets map[string]string, now time.Time) error {
	defer s.lock()()
	for ref, pushed := range targets {
		s.trigger(name, ref, -1, 0, now)
		r := s.refs[refKey{name, ref}]
		r.Pushed, r.PushedSet = pushed, true
	}
	s.mirrors[name] = true
	return nil
}

func (s *Store) HasMirror(_ context.Context, name string) (bool, error) {
	defer s.lock()()
	return s.mirrors[name], nil
}

func (s *Store) Seed(_ context.Context, name string) (string, error) {
	defer s.lock()()
	return s.seeds[name], nil
}

func (s *Store) SetSeed(_ context.Context, name, state string) error {
	defer s.lock()()
	s.seeds[name] = state
	return nil
}

func (s *Store) SeedRefs(_ context.Context, name string) (map[string]mirror.SeedRef, error) {
	defer s.lock()()
	out := map[string]mirror.SeedRef{}
	for k, r := range s.seedRefs {
		if k.mirror == name {
			out[k.ref] = r
		}
	}
	return out, nil
}

func (s *Store) SetSeedRef(_ context.Context, name, ref string, r mirror.SeedRef) error {
	defer s.lock()()
	s.seedRefs[refKey{name, ref}] = r
	return nil
}

func (s *Store) SeedCleaned(_ context.Context, name string) (bool, error) {
	defer s.lock()()
	return s.cleaned[name], nil
}

func (s *Store) FinishSeedCleanup(_ context.Context, name string, refs []string) error {
	defer s.lock()()
	for _, ref := range refs {
		delete(s.refs, refKey{name, ref})
		delete(s.seedRefs, refKey{name, ref})
	}
	s.cleaned[name] = true
	return nil
}

func (s *Store) StateRefs(_ context.Context, name string) ([]string, error) {
	defer s.lock()()
	var out []string
	for _, r := range s.sortedRefs(name) {
		out = append(out, r.Name)
	}
	return out, nil
}

func (s *Store) Refs(_ context.Context, name, after string, limit int) ([]mirror.Ref, error) {
	defer s.lock()()
	var out []mirror.Ref
	for _, r := range s.sortedRefs(name) {
		if r.Name > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Store) Stats(context.Context) ([]mirror.Stats, error) {
	defer s.lock()()
	byName := map[string]*mirror.Stats{}
	get := func(n string) *mirror.Stats {
		if byName[n] == nil {
			byName[n] = &mirror.Stats{Mirror: n, Failing: map[mirror.Class]int{}}
		}
		return byName[n]
	}
	for n := range s.mirrors {
		get(n).Reconciled = true
	}
	for k, r := range s.refs {
		st := get(k.mirror)
		if r.Dirty {
			st.Dirty++
			if st.OldestDirty.IsZero() || r.DirtySince.Before(st.OldestDirty) {
				st.OldestDirty = r.DirtySince
			}
		}
		if r.Failure != "" {
			st.Failing[r.Failure]++
		}
	}
	var out []mirror.Stats
	for _, n := range slices.Sorted(maps.Keys(byName)) {
		out = append(out, *byName[n])
	}
	return out, nil
}
