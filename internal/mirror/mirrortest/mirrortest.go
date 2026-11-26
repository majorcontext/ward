// Package mirrortest is the conformance suite of mirror.Store.
package mirrortest

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/majorcontext/motte/api"

	"github.com/majorcontext/ward/internal/mirror"
)

var at = time.Unix(2_000_000, 0).UTC()

func hit(partition int, seq int64, ref string, mirrors ...string) mirror.Hit {
	return mirror.Hit{Event: api.Event{Partition: partition, Seq: seq, Ref: ref}, Mirrors: mirrors}
}

// Run runs the suite on a new Store from newStore for each case.
func Run(t *testing.T, newStore func(t *testing.T) mirror.Store) {
	ctx := context.Background()
	refOf := func(t *testing.T, s mirror.Store, m, name string) mirror.Ref {
		t.Helper()
		refs, err := s.Refs(ctx, m, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range refs {
			if r.Name == name {
				return r
			}
		}
		t.Fatalf("ref %s/%s not found", m, name)
		return mirror.Ref{}
	}
	t.Run("a hook compares its sequence with its partition", func(t *testing.T) {
		s := newStore(t)
		for _, c := range []struct {
			seq  int64
			want mirror.HookResult
		}{{5, mirror.HookBaseline}, {6, mirror.HookInOrder}, {6, mirror.HookRepeat}, {4, mirror.HookRepeat}, {9, mirror.HookGap}} {
			if got, err := s.RecordHook(ctx, hit(1, c.seq, "main", "m"), at); err != nil || got != c.want {
				t.Fatalf("seq %d: %v, %v; want %v", c.seq, got, err, c.want)
			}
		}
		svc, err := s.Service(ctx)
		if err != nil || svc.LastSeq[1] != 9 {
			t.Fatalf("service = %+v, %v", svc, err)
		}
		if r := refOf(t, s, "m", "main"); r.Version != 3 || !r.Dirty || r.LastSeq != 9 {
			t.Fatalf("ref = %+v; a repeated hook must not trigger", r)
		}
	})
	t.Run("a subscription replaces the cursor and can clear the sequences", func(t *testing.T) {
		s := newStore(t)
		s.RecordHook(ctx, hit(1, 5, "main", "m"), at)
		if err := s.SetSubscription(ctx, "sub", "c1", false); err != nil {
			t.Fatal(err)
		}
		if svc, _ := s.Service(ctx); svc.SubscriptionID != "sub" || svc.EventsCursor != "c1" || svc.LastSeq[1] != 5 {
			t.Fatalf("service = %+v", svc)
		}
		s.SetSubscription(ctx, "sub2", "c2", true)
		if svc, _ := s.Service(ctx); svc.SubscriptionID != "sub2" || len(svc.LastSeq) != 0 {
			t.Fatalf("service after clear = %+v", svc)
		}
	})
	t.Run("a page stores the highest sequence, the triggers and the cursor", func(t *testing.T) {
		s := newStore(t)
		s.SetSubscription(ctx, "sub", "c0", false)
		if err := s.RecordPage(ctx, []mirror.Hit{hit(1, 7, "a", "m"), hit(1, 3, "b", "m")}, "c1", at); err != nil {
			t.Fatal(err)
		}
		svc, _ := s.Service(ctx)
		if svc.EventsCursor != "c1" || svc.LastSeq[1] != 7 {
			t.Fatalf("service = %+v", svc)
		}
		if names, _ := s.StateRefs(ctx, "m"); !slices.Equal(names, []string{"a", "b"}) {
			t.Fatalf("refs = %v", names)
		}
	})
	t.Run("a push clears dirty only for the version it read", func(t *testing.T) {
		s := newStore(t)
		s.RecordHook(ctx, hit(1, 1, "main", "m"), at)
		s.RecordHook(ctx, hit(1, 2, "main", "m"), at.Add(time.Second))
		r := refOf(t, s, "m", "main")
		if r.Version != 2 || !r.DirtySince.Equal(at) || r.PushedSet {
			t.Fatalf("ref = %+v", r)
		}
		if err := s.RecordPush(ctx, "m", "main", 1, "sha", at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if r = refOf(t, s, "m", "main"); !r.Dirty || r.Pushed != "sha" || !r.PushedSet || !r.PushedAt.Equal(at.Add(time.Minute)) {
			t.Fatalf("after a stale push = %+v", r)
		}
		s.RecordPush(ctx, "m", "main", 2, "sha2", at.Add(time.Minute))
		if r = refOf(t, s, "m", "main"); r.Dirty || r.Pushed != "sha2" {
			t.Fatalf("after the current push = %+v", r)
		}
	})
	t.Run("a failure counts, keeps its first time and moves the retry only for the current version", func(t *testing.T) {
		s := newStore(t)
		s.RecordHook(ctx, hit(1, 1, "main", "m"), at)
		later := at.Add(time.Minute)
		s.RecordFailure(ctx, "m", "main", 1, mirror.ClassRemoteAuth, "no", later, at.Add(time.Second))
		s.RecordFailure(ctx, "m", "main", 0, mirror.ClassLocal, "stale", later.Add(time.Hour), at.Add(2*time.Second))
		r := refOf(t, s, "m", "main")
		if r.Attempts != 2 || r.Failure != mirror.ClassLocal || r.Message != "stale" || !r.FailureSince.Equal(at.Add(time.Second)) || !r.NextAttempt.Equal(later) {
			t.Fatalf("ref = %+v", r)
		}
		s.RecordPush(ctx, "m", "main", 1, "sha", later)
		if r = refOf(t, s, "m", "main"); r.Failure != "" || r.Attempts != 0 || !r.FailureSince.IsZero() {
			t.Fatalf("a push must clear the failure: %+v", r)
		}
	})
	t.Run("due lists the dirty refs that may run and the next retry", func(t *testing.T) {
		s := newStore(t)
		s.RecordHook(ctx, hit(1, 1, "b", "m"), at)
		s.RecordHook(ctx, hit(1, 2, "a", "m"), at)
		s.RecordHook(ctx, hit(1, 3, "c", "m"), at)
		s.RecordHook(ctx, hit(1, 4, "z", "other"), at)
		s.RecordFailure(ctx, "m", "c", 1, mirror.ClassMotte, "x", at.Add(time.Minute), at)
		due, next, err := s.Due(ctx, "m", at)
		if err != nil || len(due) != 2 || due[0].Name != "a" || due[1].Name != "b" || !next.Equal(at.Add(time.Minute)) {
			t.Fatalf("due = %+v, next %v, %v", due, next, err)
		}
		s.RecordPush(ctx, "m", "a", 1, "s", at)
		if due, _, _ = s.Due(ctx, "m", at.Add(time.Hour)); len(due) != 2 {
			t.Fatalf("due later = %+v", due)
		}
	})
	t.Run("reconcile triggers each target, sets pushed and records the mirror", func(t *testing.T) {
		s := newStore(t)
		if has, _ := s.HasMirror(ctx, "m"); has {
			t.Fatal("mirror known before a reconcile")
		}
		if err := s.Reconciled(ctx, "m", map[string]string{"a": "sa", "b": ""}, at); err != nil {
			t.Fatal(err)
		}
		if has, _ := s.HasMirror(ctx, "m"); !has {
			t.Fatal("mirror unknown after a reconcile")
		}
		a, b := refOf(t, s, "m", "a"), refOf(t, s, "m", "b")
		if !a.Dirty || a.Pushed != "sa" || !a.PushedSet || !b.PushedSet || b.Pushed != "" || a.LastPartition != -1 {
			t.Fatalf("a = %+v, b = %+v", a, b)
		}
		s.SetPushed(ctx, "m", "a", "sa2")
		if got := refOf(t, s, "m", "a"); got.Pushed != "sa2" || !got.Dirty {
			t.Fatalf("after SetPushed = %+v", got)
		}
	})
	t.Run("the seed records a state, refs and a cleanup that removes them", func(t *testing.T) {
		s := newStore(t)
		if st, err := s.Seed(ctx, "m"); err != nil || st != "" {
			t.Fatalf("seed = %q, %v", st, err)
		}
		s.SetSeed(ctx, "m", "started")
		s.SetSeedRef(ctx, "m", "a", mirror.SeedRef{Commit: "c1", Next: "c2"})
		s.SetSeedRef(ctx, "m", "b", mirror.SeedRef{Commit: "c3"})
		s.SetSeedRef(ctx, "m", "a", mirror.SeedRef{Commit: "c2"})
		s.Reconciled(ctx, "m", map[string]string{"a": "", "keep": ""}, at)
		want := map[string]mirror.SeedRef{"a": {Commit: "c2"}, "b": {Commit: "c3"}}
		if st, _ := s.Seed(ctx, "m"); st != "started" {
			t.Fatalf("seed = %q", st)
		}
		if got, err := s.SeedRefs(ctx, "m"); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("seed refs = %v, %v", got, err)
		}
		if done, _ := s.SeedCleaned(ctx, "m"); done {
			t.Fatal("cleaned before the cleanup")
		}
		if err := s.FinishSeedCleanup(ctx, "m", []string{"a"}); err != nil {
			t.Fatal(err)
		}
		if done, _ := s.SeedCleaned(ctx, "m"); !done {
			t.Fatal("not cleaned after the cleanup")
		}
		if got, _ := s.SeedRefs(ctx, "m"); !reflect.DeepEqual(got, map[string]mirror.SeedRef{"b": {Commit: "c3"}}) {
			t.Fatalf("seed refs after = %v", got)
		}
		if names, _ := s.StateRefs(ctx, "m"); !slices.Equal(names, []string{"keep"}) {
			t.Fatalf("state refs after = %v", names)
		}
	})
	t.Run("refs page by name within one mirror", func(t *testing.T) {
		s := newStore(t)
		s.Reconciled(ctx, "m", map[string]string{"c": "", "a": "", "b": ""}, at)
		s.Reconciled(ctx, "other", map[string]string{"a": ""}, at)
		page, err := s.Refs(ctx, "m", "a", 1)
		if err != nil || len(page) != 1 || page[0].Name != "b" {
			t.Fatalf("page = %+v, %v", page, err)
		}
	})
	t.Run("stats count dirty and failing refs of each mirror", func(t *testing.T) {
		s := newStore(t)
		s.Reconciled(ctx, "m", map[string]string{"a": "", "b": ""}, at)
		s.RecordHook(ctx, hit(1, 1, "late", "m"), at.Add(time.Hour))
		s.RecordPush(ctx, "m", "a", 1, "s", at)
		s.RecordFailure(ctx, "m", "b", 1, mirror.ClassRemoteRejected, "x", at, at)
		s.RecordHook(ctx, hit(1, 2, "z", "n"), at)
		got, err := s.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		by := map[string]mirror.Stats{}
		for _, st := range got {
			by[st.Mirror] = st
		}
		m := by["m"]
		if !m.Reconciled || m.Dirty != 2 || m.Failing[mirror.ClassRemoteRejected] != 1 || !m.OldestDirty.Equal(at) {
			t.Fatalf("m = %+v", m)
		}
		if n := by["n"]; n.Reconciled || n.Dirty != 1 {
			t.Fatalf("n = %+v", n)
		}
	})
}
