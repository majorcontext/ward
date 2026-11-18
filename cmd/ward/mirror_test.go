package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/motte/meta"

	"github.com/majorcontext/ward/internal/mirror"
	"github.com/majorcontext/ward/internal/token"
)

func (h *harness) waitTip(ref, want string) {
	h.t.Helper()
	h.waitFor(ref+" at "+want, func() bool { return h.remote.tip("refs/heads/"+ref) == want })
}

func (h *harness) waitRef(p *wardProc, ref string, ok func(refView) bool) {
	h.t.Helper()
	h.waitFor("status of "+ref, func() bool {
		r, found := p.status().ref(ref)
		return found && ok(r)
	})
}

// sameRefPartition returns two ref names of the repository that share an event partition.
func sameRefPartition(repo string) (string, string) {
	seen := map[int]string{}
	for i := range 200 {
		name := fmt.Sprintf("feature/n%d", i)
		p := meta.Partition(repo, name)
		if other, ok := seen[p]; ok {
			return other, name
		}
		seen[p] = name
	}
	panic("no two refs share a partition")
}

func TestMirrorFollowsRefsThroughTheirLife(t *testing.T) {
	h := newHarness(t)
	p := h.startWard(wardOpts{refs: []string{"main", "feature/*"}})
	h.skip()
	main, err := h.readClient().Ref(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	h.waitTip("main", main.Commit)

	c1 := h.commit("a.txt", "1")
	h.push("feature/a", c1, false)
	h.relay(p)
	h.waitTip("feature/a", c1)

	c2 := h.commit("a.txt", "2")
	h.push("feature/a", c2, false)
	h.relay(p)
	h.waitTip("feature/a", c2)

	h.git("commit", "--amend", "-q", "-m", "rewritten")
	c3 := h.git("rev-parse", "HEAD")
	h.push("feature/a", c3, true)
	h.relay(p)
	h.waitTip("feature/a", c3)
	h.waitRef(p, "feature/a", func(r refView) bool { return r.State == "synced" && r.Pushed == c3 })

	h.deleteRef("feature/a")
	h.relay(p)
	h.waitTip("feature/a", "")
	h.waitRef(p, "feature/a", func(r refView) bool { return r.State == "synced" && r.Pushed == "" })

	h.push("feature/b", c3, false)
	h.relay(p)
	h.waitTip("feature/b", c3)
	gitIn(t, h.remote.dir, "update-ref", "-d", "refs/heads/feature/b")
	n := h.count("reconcile")
	if code, _ := p.do("POST", "/v1/mirrors/"+mirrorName+"/reconcile", h.token(token.ScopeOperator, ""), nil, nil); code != http.StatusAccepted {
		t.Fatalf("reconcile answered %d", code)
	}
	h.waitFor("reconcile", func() bool { return h.count("reconcile") > n })
	h.waitTip("feature/b", c3)

	clone, _ := filepath.Glob(filepath.Join(h.dataDir, "clones", "*.git"))
	if len(clone) != 1 {
		t.Fatalf("clones = %v", clone)
	}
	gitIn(t, clone[0], "cat-file", "-e", c3)
	b := h.count("batch")
	h.deleteRef("feature/b")
	h.relay(p)
	h.waitFor("the delete batch", func() bool { return h.count("batch") > b && h.remote.tip("refs/heads/feature/b") == "" })
	if err := gitErr(clone[0], "cat-file", "-e", c3); err == nil {
		t.Fatal("the commit of the deleted ref is still in the clone")
	}
}

func TestMirrorReceiverRefusesForgeriesIgnoresRepeatsAndUnmatchedRefsAndKeepsOrder(t *testing.T) {
	h := newHarness(t)
	p := h.startWard(wardOpts{refs: []string{"feature/*"}})
	h.skip()
	c1 := h.commit("a.txt", "1")
	h.push("feature/a", c1, false)
	ev := h.events()[0]
	body, _ := json.Marshal(ev)
	good := h.sign(body)
	for name, sig := range map[string]string{
		"wrong secret": strings.Replace(good, good[len(good)-4:], "0000", 1), "no prefix": strings.TrimPrefix(good, "sha256="), "empty": "",
	} {
		code, out := p.do("POST", "/v1/hooks/motte", "", body, map[string]string{"X-Motte-Signature": sig})
		if code != http.StatusUnauthorized || !strings.Contains(string(out), `"code":"unauthorized"`) {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] = '9'
	if code, _ := p.do("POST", "/v1/hooks/motte", "", tampered, map[string]string{"X-Motte-Signature": good}); code != http.StatusUnauthorized {
		t.Errorf("tampered body answered %d", code)
	}
	if code, _ := p.do("POST", "/v1/hooks/motte", "", make([]byte, 64<<10+1), nil); code != http.StatusBadRequest {
		t.Errorf("oversize body answered %d, want 400", code)
	}
	if _, found := p.status().ref("feature/a"); found {
		t.Fatal("a forged delivery stored a trigger")
	}
	if got := p.metric(`ward_hook_events_total{result="bad_signature"}`); got != 4 {
		t.Fatalf("bad_signature = %v, want 4", got)
	}
	if code := p.deliver(ev); code != http.StatusNoContent {
		t.Fatalf("the signed event answered %d", code)
	}
	h.waitTip("feature/a", c1)
	if code := p.deliver(ev); code != http.StatusNoContent {
		t.Fatalf("a repeat answered %d, want 204", code)
	}
	if got := p.metric(`ward_hook_events_total{result="repeat"}`); got != 1 {
		t.Fatalf("repeat events = %v, want 1", got)
	}

	h.push("other/x", c1, false)
	h.push("feature/y", c1, false)
	h.relay(p)
	h.waitTip("feature/y", c1)
	if got := h.remote.tip("refs/heads/other/x"); got != "" {
		t.Fatalf("unmatched ref reached the remote at %s", got)
	}
	if _, found := p.status().ref("other/x"); found {
		t.Fatal("an unmatched ref has state")
	}

	var last string
	for i := range 5 {
		last = h.commit("f.txt", fmt.Sprint(i))
		h.push("feature/x", last, true)
	}
	evs := h.events()
	if len(evs) != 5 {
		t.Fatalf("motte holds %d events for the ref, want 5", len(evs))
	}
	for i := len(evs) - 1; i >= 0; i-- {
		p.deliver(evs[i])
	}
	h.waitTip("feature/x", last)
}

func TestMirrorReportsRemoteFailuresAndRecovers(t *testing.T) {
	h := newHarness(t)
	h.remote.rejectRef("refs/heads/feature/bad")
	p := h.startWard(wardOpts{})
	h.skip()
	c1 := h.commit("a.txt", "1")
	h.push("feature/bad", c1, false)
	h.push("feature/good", c1, false)
	h.relay(p)
	h.waitTip("feature/good", c1)
	h.waitRef(p, "feature/bad", func(r refView) bool { return r.State == "failing" && r.Failure == "remote_rejected" && r.Attempts >= 1 })
	if got := h.remote.tip("refs/heads/feature/bad"); got != "" {
		t.Fatalf("the rejected ref is at %q", got)
	}
	if r, _ := p.status().ref("feature/bad"); strings.Contains(r.Message, "refused\n") || r.Message == "" {
		t.Fatalf("message = %q", r.Message)
	}
	if v := p.status(); v.State != "failing" {
		t.Fatalf("mirror state = %s, want failing", v.State)
	}
	h.remote.allowAll()
	c2 := h.commit("a.txt", "2")
	h.push("feature/bad", c2, false)
	h.relay(p)
	h.waitTip("feature/bad", c2)
	h.waitRef(p, "feature/bad", func(r refView) bool { return r.State == "synced" && r.Failure == "" && r.Attempts == 0 })

	h.remote.pass.set("rotated")
	c3 := h.commit("a.txt", "3")
	h.push("feature/a", c3, false)
	h.relay(p)
	h.waitRef(p, "feature/a", func(r refView) bool { return r.Failure == "remote_auth" })
	h.writeFile("remote.cred", "rotated\n")
	c4 := h.commit("a.txt", "4")
	h.push("feature/a", c4, false)
	h.relay(p)
	h.waitTip("feature/a", c4)

	h.remote.srv.Close()
	h.push("feature/c", h.commit("a.txt", "5"), false)
	h.relay(p)
	h.waitRef(p, "feature/c", func(r refView) bool { return r.Failure == "remote_unreachable" })
}

func TestMirrorNeverTouchesRefsThatNoPatternMatchesAnyMore(t *testing.T) {
	h := newHarness(t)
	clk := newFakeClock(func() { h.note("timer", "") })
	h.remote.rejectRef("refs/heads/feature/a")
	p := h.startWard(wardOpts{clock: clk})
	h.skip()
	c1 := h.commit("a.txt", "1")
	h.push("feature/a", c1, false)
	h.relay(p)
	h.waitRef(p, "feature/a", func(r refView) bool { return r.Failure == "remote_rejected" })
	p.stop()

	h.remote.allowAll()
	p = h.startWard(wardOpts{refs: []string{"feature/keep"}, clock: clk})
	clk.advance(time.Minute)
	h.waitRef(p, "feature/a", func(r refView) bool { return r.State == "synced" })
	if got := h.remote.tip("refs/heads/feature/a"); got != "" {
		t.Fatalf("an excluded ref was pushed to %s", got)
	}

	orphan := h.remote.orphan()
	h.remote.setRef("refs/heads/feature/a", orphan)
	h.deleteRef("feature/a")
	n := h.count("reconcile")
	p.do("POST", "/v1/mirrors/"+mirrorName+"/reconcile", h.token(token.ScopeOperator, ""), nil, nil)
	h.waitFor("reconcile", func() bool { return h.count("reconcile") > n })
	h.push("feature/keep", c1, false)
	h.relay(p)
	h.waitTip("feature/keep", c1)
	if got := h.remote.tip("refs/heads/feature/a"); got != orphan {
		t.Fatalf("an excluded ref changed to %q", got)
	}
}

func TestMirrorRunsAnotherReconcileWhenOneIsRequestedDuringAPass(t *testing.T) {
	h := newHarness(t)
	p := h.startWard(wardOpts{})
	op := h.token(token.ScopeOperator, "")
	n := h.count("reconcile")
	entered, release := h.remote.hold()
	p.do("POST", "/v1/mirrors/"+mirrorName+"/reconcile", op, nil, nil)
	select {
	case <-entered:
	case <-time.After(20 * time.Second): //lint:allow wallclock a wait on another process that no bubble holds
		t.Fatal("the first pass never listed the remote")
	}
	p.do("POST", "/v1/mirrors/"+mirrorName+"/reconcile", op, nil, nil)
	release()
	h.waitFor("two passes", func() bool { return h.count("reconcile") >= n+2 })
}

func TestMirrorCatchesUpAfterASequenceGap(t *testing.T) {
	h := newHarness(t)
	a, b := sameRefPartition(h.repo)
	p := h.startWard(wardOpts{refs: []string{a, b}})
	h.skip()
	c1 := h.commit("a.txt", "1")
	h.push(a, c1, false)
	h.relay(p)
	h.waitTip(a, c1)

	c2 := h.commit("a.txt", "2")
	h.push(b, c2, false)
	h.skip()
	c3 := h.commit("a.txt", "3")
	h.push(a, c3, false)
	h.relay(p)
	h.waitTip(b, c2)
	h.waitTip(a, c3)
	if got := p.metric(`ward_hook_events_total{result="gap"}`); got != 1 {
		t.Fatalf("gap events = %v, want 1", got)
	}
}

func TestMirrorCatchesUpAtStartAfterDowntime(t *testing.T) {
	h := newHarness(t)
	p := h.startWard(wardOpts{})
	h.skip()
	c1 := h.commit("a.txt", "1")
	h.push("feature/a", c1, false)
	h.relay(p)
	h.waitTip("feature/a", c1)
	p.stop()

	c2 := h.commit("a.txt", "2")
	h.push("feature/a", c2, false)
	h.push("feature/b", c2, false)
	h.startWard(wardOpts{})
	h.waitTip("feature/a", c2)
	h.waitTip("feature/b", c2)
}

func TestMirrorResetsAfterMotteLosesTheEvents(t *testing.T) {
	h := newHarness(t)
	p := h.startWard(wardOpts{})
	h.skip()
	before, err := h.operatorClient().Subscriptions(t.Context())
	if err != nil || len(before) != 1 {
		t.Fatalf("subscriptions = %v, %v", before, err)
	}
	p.stop()

	c1 := h.commit("a.txt", "1")
	h.push("feature/r", c1, false)
	if err := h.motte.Meta.PruneEvents(t.Context(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	resets := h.count("reset")
	p = h.startWard(wardOpts{})
	h.waitTip("feature/r", c1)
	h.waitFor("reset", func() bool { return h.count("reset") > resets })
	after, err := h.operatorClient().Subscriptions(t.Context())
	if err != nil || len(after) != 1 || after[0].ID == before[0].ID {
		t.Fatalf("subscriptions after reset = %v, %v (before %v)", after, err, before)
	}
	h.cursor = after[0].Cursor
	c2 := h.commit("a.txt", "2")
	h.push("feature/r", c2, false)
	h.relay(p)
	h.waitTip("feature/r", c2)
}

func TestMirrorHandlesARemoteMovedOutsideWard(t *testing.T) {
	h := newHarness(t)
	p := h.startWard(wardOpts{})
	h.skip()
	c1 := h.commit("a.txt", "1")
	h.push("feature/a", c1, false)
	h.relay(p)
	h.waitTip("feature/a", c1)

	h.remote.setRef("refs/heads/feature/a", h.remote.orphan())
	c2 := h.commit("a.txt", "2")
	h.push("feature/a", c2, false)
	h.relay(p)
	h.waitTip("feature/a", c2)
	h.waitRef(p, "feature/a", func(r refView) bool { return r.State == "synced" && r.Failure == "" })
	if got := p.metric(`ward_mirror_pushes_total{mirror="m1",result="stale"}`); got != 1 {
		t.Fatalf("stale pushes = %v, want 1", got)
	}

	h.remote.setRef("refs/heads/feature/b", h.remote.orphan())
	h.push("feature/b", c2, false)
	h.relay(p)
	h.waitTip("feature/b", c2)
}

func TestMirrorBacksOffOnTheInjectedClockAndTakesNewsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.remote.rejectRef("refs/heads/feature/bad")
	clk := newFakeClock(func() { h.note("timer", "") })
	p := h.startWard(wardOpts{clock: clk})
	h.skip()
	h.push("feature/bad", h.commit("a.txt", "1"), false)
	h.relay(p)
	h.waitRef(p, "feature/bad", func(r refView) bool { return r.Attempts == 1 })
	h.waitFor("a retry timer", func() bool { return clk.pending() >= 1 })

	clk.advance(500 * time.Millisecond)
	if r, _ := p.status().ref("feature/bad"); r.Attempts != 1 {
		t.Fatalf("attempts = %d before the backoff ends", r.Attempts)
	}
	clk.advance(1500 * time.Millisecond)
	h.waitRef(p, "feature/bad", func(r refView) bool { return r.Attempts == 2 })
	h.waitFor("a second retry timer", func() bool { return clk.pending() >= 1 })

	h.remote.allowAll()
	c2 := h.commit("a.txt", "2")
	h.push("feature/bad", c2, false)
	h.relay(p)
	h.waitTip("feature/bad", c2)
	h.waitRef(p, "feature/bad", func(r refView) bool { return r.State == "synced" })
}

func TestMirrorPrunesUnknownRemoteRefsOnlyWhenAsked(t *testing.T) {
	for _, prune := range []bool{false, true} {
		t.Run(fmt.Sprint("prune_unknown=", prune), func(t *testing.T) {
			h := newHarness(t)
			stray := h.remote.orphan()
			h.remote.setRef("refs/heads/feature/stray", stray)
			h.remote.setRef("refs/heads/other/keep", stray)
			p := h.startWard(wardOpts{prune: prune})
			h.skip()
			c1 := h.commit("a.txt", "1")
			h.push("feature/a", c1, false)
			h.relay(p)
			h.waitTip("feature/a", c1)
			want := stray
			if prune {
				want = ""
			}
			h.waitTip("feature/stray", want)
			if got := h.remote.tip("refs/heads/other/keep"); got != stray {
				t.Fatalf("an unmatched remote ref changed to %q", got)
			}
		})
	}
}

func TestMirrorDeletesIdleAndOverBudgetClonesAndFetchesAgain(t *testing.T) {
	for name, o := range map[string]wardOpts{"idle ttl": {ttl: "1ns"}, "disk budget": {budget: "1"}} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			p := h.startWard(o)
			h.skip()
			dirs := func() []string { d, _ := filepath.Glob(filepath.Join(h.dataDir, "clones", "*")); return d }
			for i := range 2 {
				n := h.count("batch")
				c := h.commit("a.txt", fmt.Sprint(i))
				h.push("feature/a", c, false)
				h.relay(p)
				h.waitFor("the batch", func() bool { return h.count("batch") > n && h.remote.tip("refs/heads/feature/a") == c })
				if got := dirs(); len(got) != 0 {
					t.Fatalf("clones left after batch %d: %v", i, got)
				}
			}
		})
	}
}

func TestServiceAnswersWithTheErrorEnvelopeAndChecksTokens(t *testing.T) {
	h := newHarness(t)
	p := h.startWard(wardOpts{})
	op, read := h.token(token.ScopeOperator, ""), h.token(token.ScopeRead, h.repo)
	other := h.token(token.ScopeRead, "native:other")
	path := "/v1/mirrors/" + mirrorName
	tests := []struct {
		name, method, path, tok string
		want                    int
		code                    string
	}{
		{"health needs no token", "GET", "/health", "", 200, ""},
		{"ready needs no token", "GET", "/ready", "", 200, ""},
		{"metrics needs no token", "GET", "/metrics", "", 200, ""},
		{"unknown path", "GET", "/nope", "", 404, "not_found"},
		{"wrong method", "DELETE", "/v1/mirrors", op, 405, "method_not_allowed"},
		{"no token", "GET", "/v1/mirrors", "", 401, "unauthorized"},
		{"garbage token", "GET", "/v1/mirrors", "wd1.x.y", 401, "unauthorized"},
		{"list as operator", "GET", "/v1/mirrors", op, 200, ""},
		{"list as read", "GET", "/v1/mirrors", read, 403, "forbidden"},
		{"get as operator", "GET", path, op, 200, ""},
		{"get own repo as read", "GET", path, read, 200, ""},
		{"get other repo as read", "GET", path, other, 403, "forbidden"},
		{"get unknown as read", "GET", "/v1/mirrors/zzz", read, 403, "forbidden"},
		{"get unknown as operator", "GET", "/v1/mirrors/zzz", op, 404, "not_found"},
		{"bad cursor", "GET", path + "?cursor=!!", op, 400, "invalid_body"},
		{"reconcile as read", "POST", path + "/reconcile", read, 403, "forbidden"},
		{"reconcile unknown", "POST", "/v1/mirrors/zzz/reconcile", op, 404, "not_found"},
		{"reset as read", "POST", "/v1/hooks/motte/reset", read, 403, "forbidden"},
		{"reset as operator", "POST", "/v1/hooks/motte/reset", op, 202, ""},
	}
	for _, tt := range tests {
		code, body := p.do(tt.method, tt.path, tt.tok, nil, nil)
		var env struct {
			Error struct {
				Code    string
				Message string
				Details any
			}
		}
		_ = json.Unmarshal(body, &env)
		if code != tt.want || (tt.code != "" && env.Error.Code != tt.code) {
			t.Errorf("%s: %d %s, want %d %s", tt.name, code, body, tt.want, tt.code)
		}
	}
	expired, _ := token.Signer{Kid: "c", Key: h.callerSK}.Mint(token.Claims{Scope: token.ScopeOperator, Exp: time.Now().Add(-time.Minute).Unix()})
	_, a := p.do("GET", "/v1/mirrors", "", nil, nil)
	_, b := p.do("GET", "/v1/mirrors", expired, nil, nil)
	if string(a) != string(b) || strings.Contains(string(b), expired) {
		t.Errorf("rejections differ: %s vs %s", a, b)
	}
	h.waitFor("the reset", func() bool { return h.count("reset") > 0 })
}

func TestConfigRefusesWhatTheSpecificationRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cred := write("cred", "x")
	key := write("k", "AAAA")
	base := map[string]string{"WARD_DATA_DIR": dir, "WARD_MOTTE_URL": "http://m", "WARD_MOTTE_KID": "k", "WARD_MOTTE_KEY_FILE": key}
	base["WARD_LISTEN"] = "127.0.0.1:0"
	seed := write("seed", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	base["WARD_MOTTE_KEY_FILE"] = seed
	mirrors := `[{"name":"a","repo":"native:r","remote":"https://h/r","refs":["main"],"credential_file":"` + cred + `"}]`
	for name, extra := range map[string]map[string]string{
		"missing data dir":      {"WARD_DATA_DIR": ""},
		"short key":             {"WARD_MOTTE_KEY_FILE": key},
		"unknown trusted scope": {"WARD_TRUSTED_KEYS": `[{"kid":"c","key":"` + strings.Repeat("A", 43) + `=","scopes":["merge"]}]`},
		"mirror without url":    {"WARD_MIRRORS": mirrors},
		"short hook secret":     {"WARD_MIRRORS": mirrors, "WARD_PUBLIC_URL": "http://w", "WARD_HOOK_SECRET_FILE": write("s", "short")},
		"bad budget":            {"WARD_CLONE_DISK_BUDGET": "-1"},
		"bad ttl":               {"WARD_CLONE_IDLE_TTL": "soon"},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := run(ctx, func(k string) string { return env[k] }, nil, mirror.Options{}, nil); err == nil {
			t.Errorf("%s: ward started", name)
		}
	}
}

func exec_(dir string, args ...string) error {
	_, err := os.Stat(dir)
	if err != nil {
		return err
	}
	return gitErr(dir, args...)
}

func TestMirrorResolvesRelativePaths(t *testing.T) {
	h := newHarness(t)
	p := h.startWard(wardOpts{relative: true})
	h.skip()
	c1 := h.commit("a.txt", "1")
	h.push("feature/a", c1, false)
	h.relay(p)
	h.waitTip("feature/a", c1)
}

func TestMirrorKeepsAReconcileRequestMadeBeforeStartFinishes(t *testing.T) {
	h := newHarness(t)
	p := h.startWard(wardOpts{})
	p.stop()
	entered, release := h.holdMotte()
	n := h.count("reconcile")
	p = h.startWard(wardOpts{proxied: true, noWait: true})
	select {
	case <-entered:
	case <-time.After(20 * time.Second): //lint:allow wallclock a wait on another process that no bubble holds
		t.Fatal("ward never asked motte for its subscriptions")
	}
	if code, _ := p.do("POST", "/v1/mirrors/"+mirrorName+"/reconcile", h.token(token.ScopeOperator, ""), nil, nil); code != http.StatusAccepted {
		t.Fatalf("reconcile answered %d", code)
	}
	release()
	h.waitFor("the reconcile", func() bool { return h.count("reconcile") > n })
}

func TestMirrorDeletesClonesOfMirrorsThatAreNoLongerConfigured(t *testing.T) {
	h := newHarness(t)
	stray := filepath.Join(h.dataDir, "clones", "0123456789abcdef.git")
	gitIn(t, h.dataDir, "init", "--bare", "-q", stray)
	p := h.startWard(wardOpts{})
	h.skip()
	c1 := h.commit("a.txt", "1")
	h.push("feature/a", c1, false)
	h.relay(p)
	h.waitFor("the batch", func() bool { return h.count("batch") > 0 && h.remote.tip("refs/heads/feature/a") == c1 })
	if _, err := os.Stat(stray); err == nil {
		t.Fatal("the clone of an unconfigured repository is still there")
	}
}
