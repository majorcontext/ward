package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/motte/client"
	_ "modernc.org/sqlite"

	"github.com/majorcontext/ward/internal/check"
	mirrorsqlite "github.com/majorcontext/ward/internal/mirror/sqlite"
)

func TestLandWaitsForAPassAndThenLandsTheCheckedTree(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.awaitLand(p, h.postLand(p, "main", "feature/x", nil).ID, "checking")

	req := run.next()
	if req.Repo != h.repo || req.Check.Name != "unit" || req.Check.TimeoutSeconds != 60 || req.Env != runnerEnv ||
		req.Tree != l.Tree || h.treeOf(req.Commit) != req.Tree || len(req.CheckHash) != 64 || strings.Join(req.Check.Run, " ") != "make test" {
		t.Fatalf("request = %+v, land = %+v", req, l)
	}
	if len(l.Checks) != 1 || l.Checks[0].Name != "unit" || l.Checks[0].State != "pending" {
		t.Fatalf("checks while waiting = %+v", l.Checks)
	}
	if h.mainTip() != base {
		t.Fatal("main moved before the check passed")
	}

	p.answer(req, "pass", "ok")
	done := h.awaitLand(p, l.ID, "landed")
	if h.treeOf(h.mainTip()) != req.Tree || done.Tip != h.mainTip() || done.Tree != req.Tree || done.Checks[0].State != "pass" {
		t.Fatalf("landed = %+v, main tree %s, checked tree %s", done, h.treeOf(h.mainTip()), req.Tree)
	}
}

func TestLandEndsOnAFailAndMovesNothing(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.postLand(p, "main", "feature/x", nil)
	req := run.next()
	p.answer(req, "fail", "2 tests failed")
	failed := h.awaitLand(p, l.ID, "failed")
	d := failed.Error.Details
	if failed.Error.Code != "check_failed" || d["check"] != "unit" || d["summary"] != "2 tests failed" || d["log_url"] != "https://logs.example/"+req.RequestID || failed.Checks[0].State != "fail" {
		t.Fatalf("land = %+v", failed)
	}
	if h.mainTip() != base {
		t.Fatal("a failed check moved main")
	}
	if _, err := h.readClient().Ref(t.Context(), "refs/ward/candidates/"+l.ID); !errors.Is(err, client.ErrNotFound) {
		t.Fatalf("candidate ref after a failed check: %v", err)
	}
}

// sameTreeBranches makes two branches of base with one tree, and a ref other at base, so that one tree is landable twice.
func (h *harness) sameTreeBranches(base string) {
	h.t.Helper()
	h.branchFrom(base, "feature/a", "c.txt", "c\n")
	h.branchFrom(base, "feature/b", "c.txt", "c\n")
	h.push("other", base, false)
}

func TestLandReusesAPassForALaterLandOfTheSameTree(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	h.sameTreeBranches(base)
	first := h.postLand(p, "main", "feature/a", nil)
	p.answer(run.next(), "pass", "ok")
	h.awaitLand(p, first.ID, "landed")

	second := h.awaitLand(p, h.postLand(p, "other", "feature/b", nil).ID, "landed")
	if second.Checks[0].State != "pass" || len(run.extra()) != 0 {
		t.Fatalf("second land = %+v, %d new requests", second, len(run.extra()))
	}
}

func TestLandAsksAgainAfterAFailAndShowsTheCheckAsFlaky(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	h.sameTreeBranches(base)
	first := h.postLand(p, "main", "feature/a", nil)
	p.answer(run.next(), "fail", "flake")
	h.awaitLand(p, first.ID, "failed")

	second := h.postLand(p, "other", "feature/b", nil)
	req := run.next()
	p.answer(req, "pass", "ok")
	done := h.awaitLand(p, second.ID, "landed")
	if done.Checks[0].State != "flaky" {
		t.Fatalf("checks = %+v, want flaky", done.Checks)
	}
}

func TestLandChecksTheNewCandidateWhenAnotherWriterMovesTheRefDuringTheChecks(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	moved := h.branchFrom(base, "feature/m", "b.txt", "b\n")
	l := h.postLand(p, "main", "feature/x", nil)
	first := run.next()
	h.awaitLand(p, l.ID, "checking")
	cur, err := h.readClient().Ref(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.motte.Client(h.motte.MergeGrant(t, h.repo)).SwapRef(t.Context(), "main", cur.Generation, moved); err != nil {
		t.Fatal(err)
	}

	p.answer(first, "pass", "ok")
	second := run.next()
	if second.Tree == first.Tree || second.RequestID == first.RequestID {
		t.Fatalf("the land asked about %+v again after the ref moved: first %+v", second, first)
	}
	if h.treeOf(h.mainTip()) == first.Tree {
		t.Fatal("the land moved the ref to a tree that the moved ref did not produce")
	}
	p.answer(second, "pass", "ok")
	done := h.awaitLand(p, l.ID, "landed")
	h.git("fetch", "-q", "origin", "main")
	if h.treeOf(h.mainTip()) != second.Tree || done.Tree != second.Tree || h.git("show", "FETCH_HEAD:b.txt") != "b" || h.git("show", "FETCH_HEAD:c.txt") != "c" {
		t.Fatalf("land = %+v, main tree %s, checked tree %s", done, h.treeOf(h.mainTip()), second.Tree)
	}
}

func TestLandSendsOneRequestForOneTreeWhenTwoLandsWait(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	h.sameTreeBranches(base)
	a, b := h.postLand(p, "main", "feature/a", nil), h.postLand(p, "other", "feature/b", nil)
	h.awaitLand(p, a.ID, "checking")
	h.awaitLand(p, b.ID, "checking")
	p.answer(run.next(), "pass", "ok")
	h.awaitLand(p, a.ID, "landed")
	h.awaitLand(p, b.ID, "landed")
	if len(run.extra()) != 0 {
		t.Fatalf("%d extra requests for one key", len(run.extra()))
	}
}

func TestLandJudgesABranchByTheCheckFileOfTheTip(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/weaken", ".ward/checks.json", `{"checks":[]}`)
	h.branchFrom(base, "feature/later", "d.txt", "d\n")
	weaken := h.postLand(p, "main", "feature/weaken", nil)
	req := run.next()
	if req.Check.Name != "unit" {
		t.Fatalf("request = %+v: the branch changed the checks that judge it", req)
	}
	p.answer(req, "pass", "ok")
	h.awaitLand(p, weaken.ID, "landed")

	later := h.awaitLand(p, h.postLand(p, "main", "feature/later", nil).ID, "landed")
	if len(later.Checks) != 0 || len(run.extra()) != 0 {
		t.Fatalf("a change to the check file did not apply to the next land: %+v", later)
	}
}

func TestLandFailsWithLandInvalidWhenTheCheckFileOrTheRunnerIsMissing(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		file    string
		runner  bool
		message string
	}{
		"a rule of the file is broken":  {`{"checks":[{"name":"Unit","run":["x"]}]}`, true, "name must be"},
		"the file has an unknown field": {`{"checks":[],"extra":1}`, true, "does not parse"},
		"no runner for the repository":  {unitChecks, false, "no runner"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLandHarness(t)
			var run *fakeRunner
			if c.runner {
				run = newRunner(t)
			}
			p := h.startWard(wardOpts{noMirror: true, runner: run})
			base := h.seedChecked(c.file)
			h.branchFrom(base, "feature/x", "c.txt", "c\n")
			failed := h.awaitLand(p, h.postLand(p, "main", "feature/x", nil).ID, "failed")
			if failed.Error.Code != "land_invalid" || !strings.Contains(failed.Error.Message, c.message) || h.mainTip() != base {
				t.Fatalf("land = %+v", failed)
			}
		})
	}
}

func TestLandResumesAfterARestartWithoutAskingAgain(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.postLand(p, "main", "feature/x", nil)
	req := run.next()
	h.awaitLand(p, l.ID, "checking")
	p.stop()

	p = h.startWard(wardOpts{noMirror: true, runner: run})
	p.answer(req, "pass", "ok")
	done := h.awaitLand(p, l.ID, "landed")
	if h.treeOf(h.mainTip()) != req.Tree || done.Tree != req.Tree {
		t.Fatalf("land = %+v", done)
	}
	for _, again := range run.extra() {
		if again.RequestID != req.RequestID {
			t.Fatalf("land = %+v, a new request %s", done, again.RequestID)
		}
	}
}

func TestResultRoutePostsOnlyForTheTokenRepositoryAndKeepsTheFirstResult(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.postLand(p, "main", "feature/x", nil)
	req := run.next()
	good := map[string]any{"request_id": req.RequestID, "status": "pass", "summary": "ok", "log_url": ""}
	for name, c := range map[string]struct {
		tok  string
		body map[string]any
		want int
	}{
		"no token":         {"", good, http.StatusUnauthorized},
		"land token":       {h.landToken(h.repo), good, http.StatusForbidden},
		"another repo":     {h.checkToken("native:other"), good, http.StatusForbidden},
		"unknown request":  {h.checkToken(h.repo), map[string]any{"request_id": "nope", "status": "pass"}, http.StatusNotFound},
		"bad status":       {h.checkToken(h.repo), map[string]any{"request_id": req.RequestID, "status": "maybe"}, http.StatusBadRequest},
		"summary too long": {h.checkToken(h.repo), map[string]any{"request_id": req.RequestID, "status": "pass", "summary": strings.Repeat("x", 4097)}, http.StatusBadRequest},
		"log url too long": {h.checkToken(h.repo), map[string]any{"request_id": req.RequestID, "status": "pass", "log_url": strings.Repeat("x", 2049)}, http.StatusBadRequest},
		"unknown field":    {h.checkToken(h.repo), map[string]any{"request_id": req.RequestID, "status": "pass", "extra": 1}, http.StatusBadRequest},
	} {
		if code, body := p.result(c.tok, c.body); code != c.want {
			t.Errorf("%s: status = %d %s, want %d", name, code, body, c.want)
		}
	}
	trailing := `{"request_id":"` + req.RequestID + `","status":"pass"} {}`
	if code, body := p.do("POST", "/v1/results", h.checkToken(h.repo), []byte(trailing), nil); code != http.StatusBadRequest {
		t.Errorf("trailing data: status = %d %s", code, body)
	}
	if _, got := p.landGet(h.landToken(h.repo), l.ID, ""); got.State != "checking" {
		t.Fatalf("a refused result changed the land: %+v", got)
	}
	p.answer(req, "pass", "ok")
	p.answer(req, "fail", "late")
	done := h.awaitLand(p, l.ID, "landed")
	if done.Checks[0].State != "pass" {
		t.Fatalf("checks = %+v: a second result for one request changed the first", done.Checks)
	}
}

func TestRequestIsLeasableAgainWhenItsLeaseEnds(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	clk, waitTimer := newTickClock(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run, clock: clk})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.postLand(p, "main", "feature/x", nil)
	first := run.next()
	if again := run.extra(); len(again) != 0 {
		t.Fatalf("a request with a live lease was leased again: %+v", again)
	}

	late := 60*time.Second + 10*time.Minute
	waitTimer(late)
	clk.advance(late)
	second := run.next()
	if second.Tree != first.Tree {
		t.Fatalf("the request after the lease ended = %+v, first = %+v", second, first)
	}
	p.answer(second, "pass", "ok")
	h.awaitLand(p, l.ID, "landed")
}

func TestLandCommandWaitsForChecksAndReportsAFailedCheck(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	h.startWard(wardOpts{noMirror: true, runner: run})
	p := &wardProc{h: h, URL: "http://" + h.addr}
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/ok", "c.txt", "c\n")
	h.branchFrom(base, "feature/bad", "d.txt", "d\n")
	env := map[string]string{"WARD_URL": p.URL, "WARD_KID": "c",
		"WARD_KEY_FILE": h.writeFile("caller.key", base64.StdEncoding.EncodeToString(h.callerSK.Seed()))}
	start := func(branch string) (*signalWriter, *bytes.Buffer, <-chan int) {
		out, errOut, done := &signalWriter{want: "checking unit\n", seen: make(chan struct{})}, &bytes.Buffer{}, make(chan int, 1)
		go func() {
			done <- runLand(t.Context(), []string{h.repo, branch}, func(k string) string { return env[k] }, time.Now, out, errOut)
		}()
		return out, errOut, done
	}

	out, errOut, done := start("feature/ok")
	req := run.next()
	<-out.seen
	p.answer(req, "pass", "ok")
	if code := <-done; code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), "landed feature/ok") {
		t.Fatalf("passing land: exit %d\nstdout: %s\nstderr: %s", code, out.String(), errOut.String())
	}

	out, errOut, done = start("feature/bad")
	req = run.next()
	<-out.seen
	p.answer(req, "fail", "boom")
	code := <-done
	for _, want := range []string{"check_failed", "check: unit\n", "summary: boom\n", "log_url: https://logs.example/" + req.RequestID + "\n"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut.String())
		}
	}
	if code != 1 || strings.Contains(out.String(), "landed") {
		t.Fatalf("failing land: exit %d\nstdout: %s", code, out.String())
	}
}

func TestLandReadEndsAtOnceForAnOldStateAtAChangeOrWhenTheWaitEnds(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	clk, waitTimer := newTickClock(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run, clock: clk})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.postLand(p, "main", "feature/x", nil)
	req := run.next()
	h.awaitLand(p, l.ID, "checking")
	read := func(wait string) <-chan landState {
		out := make(chan landState, 1)
		go func() { _, got := p.landGet(h.landToken(h.repo), l.ID, "?wait="+wait); out <- got }()
		return out
	}

	ended := read("7")
	waitTimer(7 * time.Second)
	clk.advance(7 * time.Second)
	if got := <-ended; got.State != "checking" {
		t.Fatalf("a wait that ended with no change answered %+v", got)
	}

	if _, got := p.landGet(h.landToken(h.repo), l.ID, "?wait=30&state=queued"); got.State != "checking" {
		t.Fatalf("a wait that named an old state answered %+v", got)
	}
	changed := read("31&state=checking")
	waitTimer(31 * time.Second)
	p.answer(req, "pass", "ok")
	if got := <-changed; got.State != "landing" && got.State != "landed" {
		t.Fatalf("a wait answered %+v after the state changed", got)
	}
}

func TestLandResumesCheckingWithItsStoredMarkSoAFailBeforeTheRestartEndsIt(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.postLand(p, "main", "feature/x", nil)
	req := run.next()
	h.awaitLand(p, l.ID, "checking")
	p.stop()
	st, err := mirrorsqlite.Open(t.Context(), filepath.Join(h.dataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.AddResult(t.Context(), check.Result{RequestID: req.RequestID, Repo: req.Repo, Tree: req.Tree, Hash: req.CheckHash, Env: req.Env, Status: check.StatusFail, Summary: "boom"})
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}

	p = h.startWard(wardOpts{noMirror: true, runner: run})
	failed := h.awaitLand(p, l.ID, "failed")
	if failed.Error.Code != "check_failed" || failed.Error.Details["summary"] != "boom" || len(run.extra()) != 0 {
		t.Fatalf("land = %+v, %d new requests: a fail from before the restart must end the land", failed, len(run.extra()))
	}
}

func TestLandRetriesAStoreErrorWithBackoffAndLandsWhenTheStoreRecovers(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	clk := newFakeClock(func() { h.note("timer", "") })
	logged := make(chan struct{}, 1)
	hook := func(line string) {
		if strings.Contains(line, "record the land") {
			select {
			case logged <- struct{}{}:
			default:
			}
		}
	}
	h.logHook.Store(&hook)
	p := h.startWard(wardOpts{noMirror: true, runner: run, clock: clk})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.postLand(p, "main", "feature/x", nil)
	req := run.next()
	h.awaitLand(p, l.ID, "checking")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h.dataDir, "state.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER failwrite BEFORE UPDATE ON lands
		WHEN NEW.body LIKE '%"state":"checking"%' AND NEW.body LIKE '%"state":"pass"%' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	moved := make(chan string, 1)
	go func() {
		r, _ := http.NewRequestWithContext(t.Context(), "GET", p.URL+"/v1/lands/"+l.ID+"?wait=30&state=checking", nil)
		r.Header.Set("Authorization", "Bearer "+h.landToken(h.repo))
		if resp, err := http.DefaultClient.Do(r); err == nil {
			_ = resp.Body.Close()
			moved <- "answered"
		}
	}()
	p.answer(req, "pass", "ok")
	select {
	case <-logged:
	case <-moved:
		t.Fatal("the land left checking when one write failed: a store error must not end the land")
	}
	if _, got := p.landGet(h.landToken(h.repo), l.ID, ""); got.State != "checking" || got.Error != nil {
		t.Fatalf("land after the failed write = %+v", got)
	}
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER failwrite`); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the backoff of the worker", func() bool { return clk.hasTimer(time.Second) })
	clk.advance(time.Second)
	h.awaitLand(p, l.ID, "landed")
}

// queueBehindACheck lands feature/a and waits until its check runs, and then queues the branches feature/<name> of names.
func (h *harness) queueBehindACheck(p *wardProc, run *fakeRunner, base string, names ...string) (first landState, req runnerRequest, queued []landState) {
	h.t.Helper()
	for _, n := range append([]string{"a"}, names...) {
		h.branchFrom(base, "feature/"+n, strings.TrimRight(n, "0123456789")+".txt", n+"\n")
	}
	first = h.postLand(p, "main", "feature/a", nil)
	req = run.next()
	for _, n := range names {
		queued = append(queued, h.postLand(p, "main", "feature/"+n, nil))
	}
	return first, req, queued
}

func TestLandsQueuedBehindACheckLandTogetherOnOneCheckRun(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	clk := newFakeClock(func() { h.note("timer", "") })
	p := h.startWard(wardOpts{noMirror: true, proxied: true, runner: run, clock: clk})
	base := h.seedChecked(unitChecks)
	first, req, queued := h.queueBehindACheck(p, run, base, "b", "c")
	p.answer(req, "pass", "ok")
	h.awaitLand(p, first.ID, "landed")

	batch := run.next()
	h.landTimeouts.Store(8)
	p.answer(batch, "pass", "ok")
	for range 8 {
		h.waitFor("the pause before the repeat", func() bool { return clk.hasTimer(2 * time.Second) })
		clk.advance(2 * time.Second)
	}
	b, c := h.awaitLand(p, queued[0].ID, "landed"), h.awaitLand(p, queued[1].ID, "landed")
	h.git("fetch", "-q", "origin", "main")
	h.mu.Lock()
	var sent []string
	for _, b := range h.landBodies {
		if strings.Contains(b, `-b1"`) {
			sent = append(sent, b)
		}
	}
	h.mu.Unlock()
	if len(slices.Compact(slices.Clone(sent))) != 1 || len(sent) != 9 {
		t.Fatalf("batch land calls = %q: every repeat must carry the same id and values", sent)
	}
	if len(run.extra()) != 0 || h.treeOf(h.mainTip()) != batch.Tree || c.Tip != h.mainTip() || b.Tip == c.Tip || b.Generation != c.Generation ||
		h.git("show", "FETCH_HEAD:b.txt") != "b" || h.git("show", "FETCH_HEAD:c.txt") != "c" {
		t.Fatalf("b = %+v, c = %+v, %d extra requests, main tree %s, checked tree %s", b, c, len(run.extra()), h.treeOf(h.mainTip()), batch.Tree)
	}
}

func TestLandFailsOnlyTheChangeThatBreaksTheBatchAndLandsTheRest(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	first, req, queued := h.queueBehindACheck(p, run, base, "b", "c", "d")
	p.answer(req, "pass", "ok")
	h.awaitLand(p, first.ID, "landed")

	whole := run.next()
	p.answer(whole, "fail", "c breaks it")
	prefixB := run.next()
	p.answer(prefixB, "pass", "ok")
	prefixBC := run.next()
	p.answer(prefixBC, "fail", "c breaks it")
	b, c := h.awaitLand(p, queued[0].ID, "landed"), h.awaitLand(p, queued[1].ID, "failed")
	alone := run.next()
	p.answer(alone, "pass", "ok")
	d := h.awaitLand(p, queued[2].ID, "landed")
	h.git("fetch", "-q", "origin", "main")
	if c.Error.Code != "check_failed" || c.Error.Details["summary"] != "c breaks it" || c.Checks[0].State != "fail" || b.Checks[0].State != "pass" ||
		h.git("show", "FETCH_HEAD:b.txt") != "b" || h.git("show", "FETCH_HEAD:d.txt") != "d" || d.Tip != h.mainTip() ||
		gitErr(h.work, "cat-file", "-e", "FETCH_HEAD:c.txt") == nil || len(run.extra()) != 0 {
		t.Fatalf("b = %+v, c = %+v, d = %+v", b, c, d)
	}
}

func TestLandFailsAFirstLandThatConflictsAndStillLandsTheRestOfTheBatch(t *testing.T) {
	h, run := newLandHarness(t), newRunner(t)
	p := h.startWard(wardOpts{noMirror: true, runner: run})
	base := h.seedChecked(unitChecks)
	first, req, queued := h.queueBehindACheck(p, run, base, "a2", "b", "c")
	p.answer(req, "pass", "ok")
	h.awaitLand(p, first.ID, "landed")

	conflict := h.awaitLand(p, queued[0].ID, "failed")
	p.answer(run.next(), "pass", "ok")
	h.awaitLand(p, queued[1].ID, "landed")
	h.awaitLand(p, queued[2].ID, "landed")
	h.git("fetch", "-q", "origin", "main")
	if conflict.Error.Code != "land_conflict" || h.git("show", "FETCH_HEAD:b.txt") != "b" || h.git("show", "FETCH_HEAD:c.txt") != "c" || len(run.extra()) != 0 {
		t.Fatalf("conflicting land = %+v", conflict)
	}
}
