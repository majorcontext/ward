package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/motte/client"

	"github.com/majorcontext/ward/internal/token"
)

// branchFrom commits file on top of base in the working clone and pushes it as ref.
func (h *harness) branchFrom(base, ref, file, content string) string {
	h.t.Helper()
	h.git("reset", "-q", "--hard", base)
	c := h.commit(file, content)
	h.push(ref, c, false)
	return c
}

// seedMain adds a.txt on top of the main that motte made with the repository and returns the new main.
func (h *harness) seedMain() string {
	h.t.Helper()
	h.git("fetch", "-q", "origin", "main")
	h.git("reset", "-q", "--hard", "FETCH_HEAD")
	base := h.commit("a.txt", "base\n")
	h.push("main", base, false)
	return base
}

// protectMain makes main a protected ref, so only a merge grant moves it.
func (h *harness) protectMain() {
	h.t.Helper()
	if err := h.operatorClient().SetProtectedRefs(h.t.Context(), h.repo, []string{"main"}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) landToken(repo string) string { return h.token(token.ScopeLand, repo) }

func (h *harness) landBody(branch string, extra map[string]any) []byte {
	m := map[string]any{"repo": h.repo, "branch": branch}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

func TestLandRebasesABranchOntoAProtectedMainAndRefusesOtherTokens(t *testing.T) {
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true})
	base := h.seedMain()
	feature := h.branchFrom(base, "feature/x", "c.txt", "from the branch\n")
	h.branchFrom(base, "main", "b.txt", "from main\n")
	h.protectMain()
	if gitErr(h.work, "push", "-q", "origin", feature+":refs/heads/main") == nil {
		t.Fatal("a write grant moved the protected main")
	}

	for name, tok := range map[string]string{"read": h.token(token.ScopeRead, h.repo), "check": h.checkToken(h.repo), "land for another repo": h.landToken("native:other"), "none": ""} {
		want := http.StatusForbidden
		if tok == "" {
			want = http.StatusUnauthorized
		}
		if code, body := p.do("POST", "/v1/lands", tok, h.landBody("feature/x", nil), nil); code != want {
			t.Fatalf("%s: status = %d %s, want %d", name, code, body, want)
		}
	}
	if code, body := p.do("POST", "/v1/lands", h.landToken(h.repo), h.landBody("feature/x", map[string]any{"extra": 1}), nil); code != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d %s", code, body)
	}
	if main, _ := h.readClient().Ref(t.Context(), "main"); main.Commit == feature {
		t.Fatal("a refused token landed the branch")
	}

	queued := h.postLand(p, "", "feature/x", map[string]any{"delete_branch": true})
	got := h.awaitLand(p, queued.ID, "landed")
	main, err := h.readClient().Ref(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "main" || got.Tip != main.Commit || got.Generation != main.Generation || got.FastForward || len(got.Checks) != 0 ||
		len(got.Commits) != 1 || got.Commits[0].Old != feature || got.Commits[0].New != got.Tip || !got.BranchDeleted || got.Error != nil {
		t.Fatalf("answer = %+v, main = %+v", got, main)
	}
	h.git("fetch", "-q", "origin", "main")
	for file, want := range map[string]string{"a.txt": "base", "b.txt": "from main", "c.txt": "from the branch"} {
		if out := h.git("show", "FETCH_HEAD:"+file); out != want {
			t.Fatalf("%s at the new tip = %q, want %q", file, out, want)
		}
	}
	if _, err := h.readClient().Ref(t.Context(), "feature/x"); !errors.Is(err, client.ErrNotFound) {
		t.Fatalf("branch after delete_branch: %v", err)
	}
	if _, err := h.readClient().Ref(t.Context(), "refs/ward/candidates/"+queued.ID); !errors.Is(err, client.ErrNotFound) {
		t.Fatalf("candidate ref after the land: %v", err)
	}
}

func TestLandRefusesToReadALandWithTheWrongTokenOrId(t *testing.T) {
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true})
	base := h.seedMain()
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	l := h.postLand(p, "", "feature/x", nil)
	for name, c := range map[string]struct {
		tok  string
		id   string
		want int
	}{
		"no token":      {"", l.ID, http.StatusUnauthorized},
		"read token":    {h.token(token.ScopeRead, h.repo), l.ID, http.StatusForbidden},
		"check token":   {h.checkToken(h.repo), l.ID, http.StatusForbidden},
		"other repo":    {h.landToken("native:other"), l.ID, http.StatusForbidden},
		"unknown id":    {h.landToken(h.repo), "nope", http.StatusNotFound},
		"wait too long": {h.landToken(h.repo), l.ID + "?wait=61", http.StatusBadRequest},
	} {
		if code, _ := p.landGet(c.tok, c.id, ""); code != c.want {
			t.Errorf("%s: status = %d, want %d", name, code, c.want)
		}
	}
}

func TestLandReportsAConflictWithItsPathsAndLeavesTheRefAlone(t *testing.T) {
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true})
	base := h.seedMain()
	h.branchFrom(base, "feature/bad", "a.txt", "branch edit\n")
	moved := h.branchFrom(base, "main", "a.txt", "main edit\n")
	h.protectMain()
	l := h.postLand(p, "", "feature/bad", nil)
	failed := h.awaitLand(p, l.ID, "failed")
	paths, _ := failed.Error.Details["paths"].([]any)
	if failed.Error.Code != "land_conflict" || len(paths) != 1 || paths[0] != "a.txt" {
		t.Fatalf("error = %+v", failed.Error)
	}
	if h.mainTip() != moved {
		t.Fatal("a conflict moved main")
	}
	if _, err := h.readClient().Ref(t.Context(), "refs/ward/candidates/"+l.ID); !errors.Is(err, client.ErrNotFound) {
		t.Fatalf("candidate ref after a conflict: %v", err)
	}
}

func TestLandCommandPrintsTheCommitMappingAndFailsOnConflict(t *testing.T) {
	h := newLandHarness(t)
	h.startWard(wardOpts{noMirror: true})
	base := h.seedMain()
	clean := h.branchFrom(base, "feature/clean", "c.txt", "c\n")
	bad := h.branchFrom(base, "feature/bad", "a.txt", "branch edit\n")
	moved := h.branchFrom(base, "main", "a.txt", "main edit\n")
	h.protectMain()

	env := map[string]string{"WARD_URL": "http://" + h.addr, "WARD_KID": "c",
		"WARD_KEY_FILE": h.writeFile("caller.key", base64.StdEncoding.EncodeToString(h.callerSK.Seed()))}
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := runLand(t.Context(), args, func(k string) string { return env[k] }, time.Now, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	code, out, errOut := run(h.repo, "feature/clean")
	main, _ := h.readClient().Ref(t.Context(), "main")
	if code != 0 || errOut != "" || !strings.Contains(out, "tip "+main.Commit+"\n") || !strings.Contains(out, clean+" -> "+main.Commit+"\n") {
		t.Fatalf("clean land: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if main.Commit == moved {
		t.Fatal("the land did not move main")
	}

	code, out, errOut = run(h.repo, "feature/bad")
	if code != 1 || out != "" || !strings.Contains(errOut, "land_conflict") || !strings.Contains(errOut, "\na.txt\n") {
		t.Fatalf("conflicting land: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if after, _ := h.readClient().Ref(t.Context(), "main"); after.Commit != main.Commit {
		t.Fatal("a conflict moved main")
	}
	if br, _ := h.readClient().Ref(t.Context(), "feature/bad"); br.Commit != bad {
		t.Fatal("a conflict changed the branch")
	}

	if code, _, errOut := run("--bogus", h.repo, "feature/bad"); code != 2 {
		t.Fatalf("usage error: exit %d, %s", code, errOut)
	}
	env["WARD_KEY_FILE"] = ""
	if code, _, errOut := run(h.repo, "feature/bad"); code != 2 || !slices.Contains(strings.Fields(errOut), "WARD_TOKEN,") {
		t.Fatalf("no credential: exit %d, %s", code, errOut)
	}
}

func TestLandRepeatsItsCallAfterARestartAndAcceptsTheFirstResult(t *testing.T) {
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true, proxied: true})
	base := h.seedMain()
	feature := h.branchFrom(base, "feature/x", "c.txt", "c\n")
	h.branchFrom(base, "main", "b.txt", "b\n")
	h.protectMain()
	h.holdLand.Store(true)
	l := h.postLand(p, "main", "feature/x", nil)
	<-h.landRecord
	p.stop()
	if h.mainTip() == base {
		t.Fatal("motte did not record the land before the restart")
	}
	h.holdLand.Store(false)

	p = h.startWard(wardOpts{noMirror: true, proxied: true})
	done := h.awaitLand(p, l.ID, "landed")
	if done.Tip != h.mainTip() || len(done.Commits) != 1 || done.Commits[0].Old != feature || done.Commits[0].New != done.Tip {
		t.Fatalf("land = %+v, main = %s", done, h.mainTip())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.landBodies) != 2 || h.landBodies[0] != h.landBodies[1] || !strings.Contains(h.landBodies[0], `"id":"`+l.ID+`-1"`) {
		t.Fatalf("land calls = %q: the repeat must carry the same id and values", h.landBodies)
	}
}
