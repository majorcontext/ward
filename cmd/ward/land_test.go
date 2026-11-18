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
	h := newHarness(t)
	p := h.startWard(wardOpts{})
	base := h.seedMain()
	feature := h.branchFrom(base, "feature/x", "c.txt", "from the branch\n")
	h.branchFrom(base, "main", "b.txt", "from main\n")
	h.protectMain()
	if gitErr(h.work, "push", "-q", "origin", feature+":refs/heads/main") == nil {
		t.Fatal("a write grant moved the protected main")
	}

	for name, tok := range map[string]string{"read": h.token(token.ScopeRead, h.repo), "land for another repo": h.landToken("native:other"), "none": ""} {
		want := http.StatusForbidden
		if tok == "" {
			want = http.StatusUnauthorized
		}
		if code, body := p.do("POST", "/v1/lands", tok, h.landBody("feature/x", nil), nil); code != want {
			t.Fatalf("%s: status = %d %s, want %d", name, code, body, want)
		}
	}
	if main, _ := h.readClient().Ref(t.Context(), "main"); main.Commit == feature {
		t.Fatal("a refused token landed the branch")
	}

	code, body := p.do("POST", "/v1/lands", h.landToken(h.repo), h.landBody("feature/x", map[string]any{"delete_branch": true}), nil)
	if code != http.StatusOK {
		t.Fatalf("land = %d %s", code, body)
	}
	var got landAnswer
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	main, err := h.readClient().Ref(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "main" || got.Tip != main.Commit || got.Generation != main.Generation || got.FastForward ||
		len(got.Commits) != 1 || got.Commits[0].Old != feature || got.Commits[0].New != got.Tip || !got.BranchDeleted {
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
}

func TestLandCommandPrintsTheCommitMappingAndFailsOnConflict(t *testing.T) {
	h := newHarness(t)
	h.startWard(wardOpts{})
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
	if code != 1 || out != "" || !strings.Contains(errOut, "\na.txt\n") {
		t.Fatalf("conflicting land: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if after, _ := h.readClient().Ref(t.Context(), "main"); after.Commit != main.Commit {
		t.Fatal("a conflict moved main")
	}
	if br, _ := h.readClient().Ref(t.Context(), "feature/bad"); br.Commit != bad {
		t.Fatal("a conflict changed the branch")
	}

	env["WARD_KEY_FILE"] = ""
	if code, _, errOut := run(h.repo, "feature/bad"); code != 2 || !slices.Contains(strings.Fields(errOut), "WARD_TOKEN,") {
		t.Fatalf("no credential: exit %d, %s", code, errOut)
	}
}
