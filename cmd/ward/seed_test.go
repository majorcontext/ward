package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fillRemote gives the remote main with n commits, a branch and an annotated tag, and returns the tip of main.
func (h *harness) fillRemote(n, size int) string {
	h.t.Helper()
	src := h.t.TempDir()
	gitIn(h.t, src, "init", "-q", "-b", "main")
	for i := range n {
		name := fmt.Sprintf("f%d.txt", i)
		content := name
		if size > 0 {
			content = name + strings.Repeat("0", size)
		}
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
		gitIn(h.t, src, "add", name)
		h.commitAt(src, i, name)
	}
	gitIn(h.t, src, "tag", "-a", "-m", "release", "v1")
	gitIn(h.t, src, "checkout", "-q", "-b", "feature/x")
	if err := os.WriteFile(filepath.Join(src, "x.txt"), []byte("x"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	gitIn(h.t, src, "add", "x.txt")
	h.commitAt(src, n, "x")
	gitIn(h.t, src, "push", "-q", h.remote.dir, "main", "feature/x", "refs/tags/v1")
	return h.remote.tip("refs/heads/main")
}

// commitAt commits with a committer time that grows with i, so the seed order of the refs is fixed.
func (h *harness) commitAt(dir string, i int, msg string) {
	h.t.Helper()
	cmd := exec.Command("git", "commit", "-q", "-m", msg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com", fmt.Sprintf("GIT_COMMITTER_DATE=%d +0000", 1700000000+i*60))
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("commit: %v\n%s", err, out)
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
	gitIn(t, h.remote.dir, "update-ref", "refs/heads/feature/late", h.remote.tip("refs/heads/main"))
	p = h.startWard(wardOpts{refs: seedRefs, seed: true})
	h.skip()
	h.git("fetch", "-q", "origin", "main")
	h.git("checkout", "-q", "-B", "w", "FETCH_HEAD")
	c := h.commit("new.txt", "n")
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
