package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/mirror"
	mirrorsqlite "github.com/majorcontext/ward/internal/mirror/sqlite"
)

// fillRemote gives the remote main with n commits, a branch and an annotated tag, and returns the tip of main.
// The committer time of the commits grows with their number, so the seed order of the refs is fixed.
func (h *harness) fillRemote(n, size int) string {
	h.t.Helper()
	files := map[string]string{}
	var tip string
	for i := range n {
		name := fmt.Sprintf("f%d.txt", i)
		files[name] = name
		if size > 0 {
			files[name] += strings.Repeat("0", size)
		}
		objs := commitObjs(files, tip, name, 1700000000+int64(i)*60)
		tip = objs[len(objs)-1].id
		h.fillObjects(objs)
	}
	tag := newObj("tag", fmt.Appendf(nil, "object %s\ntype commit\ntag v1\ntagger a <a@example.com> 1700000000 +0000\n\nrelease\n", tip))
	files["x.txt"] = "x"
	objs := commitObjs(files, tip, "x", 1700000000+int64(n)*60)
	h.fillObjects(append(objs, tag))
	for ref, id := range map[string]string{"refs/heads/main": tip, "refs/heads/feature/x": objs[len(objs)-1].id, "refs/tags/v1": tag.id} {
		if err := writeRef(h.remote.dir, ref, id); err != nil {
			h.t.Fatal(err)
		}
	}
	return tip
}

func (h *harness) fillObjects(objs []gitObj) {
	h.t.Helper()
	if err := writeLoose(h.remote.dir, objs); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) motteRefs() map[string]string {
	h.t.Helper()
	refs, err := h.readClient().Refs(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range refs {
		out[r.Name] = r.Commit
	}
	return out
}

var seedRefs = []string{"main", "feature/*", "refs/tags/*"}

func (h *harness) wantSeeded() {
	h.t.Helper()
	got := h.motteRefs()
	for name, git := range map[string]string{"main": "refs/heads/main", "feature/x": "refs/heads/feature/x", "refs/tags/v1": "refs/tags/v1"} {
		if got[name] == "" || got[name] != h.remote.tip(git) {
			h.t.Errorf("motte %s = %q, remote %s = %q", name, got[name], git, h.remote.tip(git))
		}
	}
}

func TestSeedCopiesTheRemoteOnceAndMirrorsBackAfterARestart(t *testing.T) {
	h := newHarness(t)
	h.http = true
	h.fillRemote(2, 0)
	hold, release := h.remote.hold()
	p := h.startWard(wardOpts{refs: seedRefs, seed: true, noWait: true})
	<-hold
	if st := p.status().State; st != "seeding" {
		t.Fatalf("state during the seed = %q, want seeding", st)
	}
	release()
	h.waitFor("the seed", func() bool { return h.count("seed") > 0 && h.count("reconcile") > 0 })
	h.wantSeeded()
	h.waitFor("a synced mirror", func() bool { return p.status().State == "synced" })
	if got := p.metric(`ward_mirror_seeds_total{mirror="m1",result="seeded"}`); got != 1 {
		t.Fatalf("seeded = %v, want 1", got)
	}

	p.stop()
	h.remote.setRef("refs/heads/feature/late", h.remote.tip("refs/heads/main"))
	p = h.startWard(wardOpts{refs: seedRefs, seed: true})
	h.skip()
	h.git("fetch", "-q", "origin", "main")
	h.git("checkout", "-q", "-B", "w", "FETCH_HEAD")
	c := h.gitCommit("new.txt", "n")
	h.push("feature/y", c, false)
	h.relay(p)
	h.waitTip("feature/y", c)
	if got := p.metric(`ward_mirror_seeds_total{mirror="m1",result="seeded"}`); got != 0 {
		t.Fatalf("a restart seeded again: %v", got)
	}
	if _, ok := h.motteRefs()["feature/late"]; ok {
		t.Fatal("a restart copied a ref of the remote into motte")
	}
}

func TestSeedIsRefusedWhenTheMotteRepositoryHasHistory(t *testing.T) {
	h := newHarness(t)
	h.fillRemote(2, 0)
	h.push("feature/a", h.commit("a.txt", "1"), false)
	before := h.motteRefs()
	p := h.startWard(wardOpts{refs: seedRefs, seed: true, noWait: true})
	h.waitFor("a refused seed", func() bool { return p.status().State == "seed_refused" })
	if got := p.metric(`ward_mirror_seeds_total{mirror="m1",result="refused"}`); got != 1 {
		t.Fatalf("refused = %v, want 1", got)
	}
	if after := h.motteRefs(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("motte refs changed: %v -> %v", before, after)
	}
	if tip := h.remote.tip("refs/heads/feature/a"); tip != "" {
		t.Fatalf("a refused seed mirrored a ref to the remote: %s", tip)
	}
}

func TestSeedFetchesWithAnInstallationToken(t *testing.T) {
	h := newHarness(t)
	h.fillRemote(1, 0)
	clk := newFakeClock(func() { h.note("timer", "") })
	gh := newFakeGitHub(t, h.remote, clk)
	h.startWard(wardOpts{refs: seedRefs, seed: true, clock: clk, app: gh})
	h.wantSeeded()
	if gh.issued() == 0 {
		t.Fatal("the seed used no installation token")
	}
}

func TestSeedPushesALongRefInSeveralPushes(t *testing.T) {
	h := newHarness(t)
	h.fillRemote(6, 0)
	h.skip()
	h.startWard(wardOpts{refs: seedRefs, seed: true, seedObjects: "7"})
	h.wantSeeded()
	main := 0
	for _, ev := range h.events() {
		if ev.Ref == "main" {
			main++
		}
	}
	if main != 3 {
		t.Fatalf("main moved %d times, want 3 pushes of 2 commits", main)
	}
}

// partialSeed starts a seed whose second push to motte fails, and returns when it waits to retry.
func (h *harness) partialSeed(clk *fakeClock) *wardProc {
	h.t.Helper()
	h.proxy()
	h.pushesLeft.Store(1)
	h.limitPushes.Store(true)
	p := h.startWard(wardOpts{refs: seedRefs, seed: true, seedObjects: "7", proxied: true, clock: clk, noWait: true})
	h.waitFor("a failed seed", func() bool { return clk.pending() >= 1 })
	return p
}

func TestSeedResumeWritesNothingToTheRemoteBeforeItFinishes(t *testing.T) {
	h := newHarness(t)
	h.fillRemote(6, 0)
	h.skip()
	h.remote.logPushes()
	p := h.partialSeed(newFakeClock(func() { h.note("timer", "") }))
	h.relay(p)
	p.stop()
	h.limitPushes.Store(false)
	p = h.startWard(wardOpts{refs: seedRefs, seed: true, seedObjects: "7", proxied: true})
	h.waitFor("the seed", func() bool { return h.count("seed") > 0 })
	h.waitFor("a synced mirror", func() bool { return p.status().State == "synced" })
	h.wantSeeded()
	if log := h.remote.pushLog(); log != "" {
		t.Fatalf("the remote was written during the seed:\n%s", log)
	}
}

func TestSeedResumeNeverOverwritesARefThatMotteMovedMeanwhile(t *testing.T) {
	h := newHarness(t)
	h.fillRemote(6, 0)
	clk := newFakeClock(func() { h.note("timer", "") })
	p := h.partialSeed(clk)
	caller := h.commit("c.txt", "caller")
	h.push("feature/x", caller, false)
	h.limitPushes.Store(false)
	clk.advance(10 * time.Minute)
	h.waitFor("a refused seed", func() bool { return p.status().State == "seed_refused" })
	if got := h.motteRefs()["feature/x"]; got != caller {
		t.Fatalf("feature/x = %s, want the commit of the caller %s", got, caller)
	}
}

func TestSeedBoundsTheInflatedSizeOfEachPush(t *testing.T) {
	h := newHarness(t)
	h.fillRemote(3, 1<<20)
	h.skip()
	h.startWard(wardOpts{refs: seedRefs, seed: true, seedInflated: fmt.Sprint(3 << 19)})
	h.wantSeeded()
	main := 0
	for _, ev := range h.events() {
		if ev.Ref == "main" {
			main++
		}
	}
	if main != 3 {
		t.Fatalf("main moved %d times, want 3 pushes of one 1 MiB file", main)
	}
}

func TestSeedRecoversFromACorruptClone(t *testing.T) {
	h := newHarness(t)
	h.fillRemote(6, 0)
	h.skip()
	clk := newFakeClock(func() { h.note("timer", "") })
	h.partialSeed(clk)
	objects, _ := filepath.Glob(filepath.Join(h.dataDir, "clones", "*.git", "objects", "??", "*"))
	for _, o := range objects {
		_ = os.Chmod(o, 0o644)
		if err := os.WriteFile(o, []byte("corrupt"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.limitPushes.Store(false)
	clk.advance(10 * time.Minute)
	h.waitFor("the seed", func() bool { return h.count("seed") > 0 })
	h.wantSeeded()
}

func TestSeedAndMirrorIgnoreEveryRefThatIsNoBranchOrTag(t *testing.T) {
	h := newHarness(t)
	h.fillRemote(2, 0)
	pull := h.remote.tip("refs/heads/main")
	h.remote.setRef("refs/pull/1/head", pull)
	h.remote.logPushes()
	p := h.startWard(wardOpts{refs: []string{"*"}, seed: true})
	h.wantSeeded()
	h.waitFor("a synced mirror", func() bool { return p.status().State == "synced" })
	if _, ok := h.motteRefs()["refs/pull/1/head"]; ok {
		t.Fatal("the seed imported a pull ref")
	}
	if log := h.remote.pushLog(); log != "" {
		t.Fatalf("ward wrote to the remote:\n%s", log)
	}
}

func TestStartDeletesOnlyTheNonMirrorableRefsThatAnEarlierSeedWrote(t *testing.T) {
	h := newHarness(t)
	h.skip()
	c := h.gitCommit("a.txt", "1")
	h.git("push", "-q", "origin", c+":refs/pull/1/head", c+":refs/pull/2/head")
	store, err := mirrorsqlite.Open(t.Context(), filepath.Join(h.dataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, step := range []error{
		store.SetSeed(t.Context(), mirrorName, "done"),
		store.SetSeedRef(t.Context(), mirrorName, "refs/pull/1/head", mirror.SeedRef{Commit: c}),
		store.Reconciled(t.Context(), mirrorName, map[string]string{"refs/pull/1/head": ""}, now),
		store.RecordFailure(t.Context(), mirrorName, "refs/pull/1/head", 1, mirror.ClassRemoteRejected, "rejected", now, now),
		store.Close(),
	} {
		if step != nil {
			t.Fatal(step)
		}
	}
	p := h.startWard(wardOpts{refs: []string{"*"}, noWait: true})
	h.waitFor("ward to start", func() bool { return h.count("init") > 0 })
	refs := h.motteRefs()
	if _, ok := refs["refs/pull/1/head"]; ok {
		t.Fatal("the pull ref of the seed is still in motte")
	}
	if refs["refs/pull/2/head"] != c {
		t.Fatalf("a ref that the seed did not write changed: %q", refs["refs/pull/2/head"])
	}
	if _, ok := p.status().ref("refs/pull/1/head"); ok {
		t.Fatal("the state still holds the pull ref")
	}
	if n := p.metric(`ward_mirror_failing_refs{class="remote_rejected",mirror="m1"}`); n != 0 {
		t.Fatalf("failing refs = %v, want 0", n)
	}
}

func TestPruneUnknownNeverDeletesARemotePullRef(t *testing.T) {
	h := newHarness(t)
	h.fillRemote(1, 0)
	pull := h.remote.tip("refs/heads/main")
	h.remote.setRef("refs/pull/1/head", pull)
	h.startWard(wardOpts{refs: []string{"*"}, prune: true})
	h.waitFor("the branch pruned", func() bool { return h.remote.tip("refs/heads/feature/x") == "" })
	if got := h.remote.tip("refs/pull/1/head"); got != pull {
		t.Fatalf("remote pull ref = %q, want %q", got, pull)
	}
}
