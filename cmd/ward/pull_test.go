package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/majorcontext/ward/internal/token"
	"github.com/majorcontext/ward/runnerproto"
)

func (p *wardProc) poll(kid string, iso runnerproto.Isolation) (int, runnerproto.Lease) {
	p.h.t.Helper()
	body, _ := json.Marshal(runnerproto.LeaseRequest{Isolation: iso})
	code, out := p.do("POST", runnerproto.LeasesPath, p.h.tokenAs(kid, token.ScopeRunner, ""), body, nil)
	var l runnerproto.Lease
	_ = json.Unmarshal(out, &l)
	return code, l
}

func (p *wardProc) resultAs(kid, requestID string) int {
	p.h.t.Helper()
	code, _ := p.result(p.h.tokenAs(kid, token.ScopeCheck, p.h.repo), map[string]any{"request_id": requestID, "status": "pass", "summary": "ok", "log_url": ""})
	return code
}

func TestPullRunnerLeasesACheckAndLandsOnItsResult(t *testing.T) {
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true, pullKids: []string{"c", "c2"}})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.awaitLand(p, h.postLand(p, "main", "feature/x", nil).ID, "checking")

	for _, c := range []struct {
		kid  string
		iso  runnerproto.Isolation
		name string
	}{{"c", runnerproto.IsolationContainer, "a weaker sandbox than the minimum"}, {"c3", runnerproto.IsolationGVisor, "an unregistered runner"}} {
		if code, _ := p.poll(c.kid, c.iso); code != http.StatusForbidden {
			t.Errorf("poll of %s = %d, want 403", c.name, code)
		}
	}
	if code, _ := p.do("POST", runnerproto.LeasesPath, h.checkToken(h.repo), []byte(`{"isolation":"gvisor"}`), nil); code != http.StatusForbidden {
		t.Errorf("poll with a check token = %d, want 403", code)
	}
	code, lease := p.poll("c", runnerproto.IsolationGVisor)
	req := lease.Request
	if code != http.StatusOK || lease.LeaseID == "" || req.Repo != h.repo || req.Tree != l.Tree || req.Env != runnerEnv || req.Check.Name != "unit" || h.treeOf(req.Commit) != req.Tree {
		t.Fatalf("poll = %d, lease %+v, land %+v", code, lease, l)
	}
	for _, kid := range []string{"c2", "c3"} {
		if code := p.resultAs(kid, req.RequestID); code != http.StatusForbidden {
			t.Errorf("result of %s, which did not lease the request = %d, want 403", kid, code)
		}
	}
	if h.mainTip() != base {
		t.Fatal("main moved before the leasing runner posted a result")
	}
	if code := p.resultAs("c", req.RequestID); code != http.StatusOK {
		t.Fatalf("result of the leasing runner = %d", code)
	}
	if done := h.awaitLand(p, l.ID, "landed"); h.treeOf(h.mainTip()) != req.Tree || done.Checks[0].State != "pass" {
		t.Fatalf("landed = %+v", done)
	}
}

func TestRunnerRevokedByARestartCannotRenewOrPostForItsLease(t *testing.T) {
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true, pullKids: []string{"c", "c2"}})
	base := h.seedChecked(unitChecks)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.awaitLand(p, h.postLand(p, "main", "feature/x", nil).ID, "checking")
	_, lease := p.poll("c", runnerproto.IsolationGVisor)
	renew := func(p *wardProc) int {
		code, _ := p.do("POST", runnerproto.RenewPath(lease.LeaseID), h.tokenAs("c", token.ScopeRunner, ""), []byte(`{}`), nil)
		return code
	}
	if code := renew(p); code != http.StatusOK {
		t.Fatalf("renew by the registered runner = %d", code)
	}
	p.stop()

	p = h.startWard(wardOpts{noMirror: true, pullKids: []string{"c2"}})
	if code := renew(p); code != http.StatusForbidden {
		t.Errorf("renew by a revoked runner = %d, want 403", code)
	}
	if code := p.resultAs("c", lease.Request.RequestID); code != http.StatusForbidden {
		t.Errorf("result of a revoked runner = %d, want 403", code)
	}
	if got := h.awaitLand(p, l.ID, "checking"); got.State != "checking" {
		t.Fatalf("land = %+v", got)
	}
}
