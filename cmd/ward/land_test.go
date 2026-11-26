package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/motte/client"

	"github.com/majorcontext/ward/internal/identity/identitytest"
	"github.com/majorcontext/ward/internal/token"
)

// branchFrom commits file on top of base and pushes it as ref.
func (h *harness) branchFrom(base, ref, file, content string) string {
	h.t.Helper()
	c := h.commitOn(base, h.with(base, file, content), "edit "+file)
	h.push(ref, c, false)
	return c
}

// seedMain adds a.txt on top of the main that motte made with the repository and returns the new main.
func (h *harness) seedMain() string {
	h.t.Helper()
	base := h.commitOn(h.mainTip(), map[string]string{"a.txt": "base\n"}, "edit a.txt")
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

// fetchAll makes the remote-tracking refs of the working tree match the refs of motte.
func (h *harness) fetchAll() {
	h.t.Helper()
	h.git("fetch", "-q", "origin", "+refs/heads/*:refs/remotes/origin/*")
}

// landBody makes the body of a land request, with the base that git finds for the branch and the ref.
func (h *harness) landBody(branch string, extra map[string]any) []byte {
	m := map[string]any{"repo": h.repo, "branch": branch}
	for k, v := range extra {
		m[k] = v
	}
	if _, ok := m["base"]; !ok {
		ref, _ := m["ref"].(string)
		if ref == "" {
			ref = "main"
		}
		h.fetchAll()
		if out, err := exec.Command("git", "-C", h.work, "merge-base", "refs/remotes/origin/"+branch, "refs/remotes/origin/"+ref).Output(); err == nil {
			m["base"] = strings.TrimSpace(string(out))
		}
	}
	b, _ := json.Marshal(m)
	return b
}

func TestLandRebasesABranchOntoAProtectedMainAndRefusesOtherTokens(t *testing.T) {
	traceInMemory()
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true})
	base := h.seedMain()
	feature := h.branchFrom(base, "feature/x", "c.txt", "from the branch\n")
	h.branchFrom(base, "main", "b.txt", "from main\n")
	h.protectMain()
	h.git("fetch", "-q", "origin", "feature/x")
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
	h.checkLandSpans(queued.ID)
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

func TestLandFindsTheBaseItselfWithoutOneAndUsesTheBaseThatTheCallerSends(t *testing.T) {
	h := newLandHarness(t)
	warned := make(chan struct{}, 4)
	hook := func(line string) {
		if strings.Contains(line, "land without a base") {
			warned <- struct{}{}
		}
	}
	h.logHook.Store(&hook)
	p := h.startWard(wardOpts{noMirror: true})
	base := h.seedMain()
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	other := h.branchFrom(base, "feature/y", "d.txt", "d\n")
	h.protectMain()
	post := func(branch string) landState {
		raw, _ := json.Marshal(map[string]any{"repo": h.repo, "branch": branch})
		code, body := p.do("POST", "/v1/lands", h.landToken(h.repo), raw, nil)
		var l landState
		if code != http.StatusAccepted || json.Unmarshal(body, &l) != nil {
			t.Fatalf("post = %d %s", code, body)
		}
		return l
	}
	for name, bad := range map[string]string{"short": "abc123", "upper case": strings.Repeat("A", 40)} {
		raw, _ := json.Marshal(map[string]string{"repo": h.repo, "branch": "feature/x", "base": bad})
		if code, body := p.do("POST", "/v1/lands", h.landToken(h.repo), raw, nil); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %s, want 400", name, code, body)
		}
	}

	old := h.awaitLand(p, post("feature/x").ID, "landed")
	select {
	case <-warned:
	default:
		t.Error("a land without a base logged no line about it")
	}
	if old.Error != nil || old.Tip == "" {
		t.Fatalf("land without a base = %+v", old)
	}
	raw, _ := json.Marshal(map[string]string{"repo": h.repo, "branch": "feature/y", "base": other})
	if code, body := p.do("POST", "/v1/lands", h.landToken(h.repo), raw, nil); code != http.StatusBadRequest || !strings.Contains(string(body), "land_invalid") {
		t.Fatalf("land with a base that the range does not reach = %d %s: the base of the caller must be used unchanged, and motte refuses it", code, body)
	}
}

func TestLandRefusesAtEnqueueAChangeThatIsTooLargeForOneLand(t *testing.T) {
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true})
	base := h.seedMain()
	tip := h.chainOn(base, map[string]string{"a.txt": "base\n"}, 1001)
	h.push("feature/big", tip, false)
	h.branchFrom(base, "feature/small", "c.txt", "c\n")
	code, body := p.do("POST", "/v1/lands", h.landToken(h.repo), h.landBody("feature/big", nil), nil)
	if code != http.StatusBadRequest || !strings.Contains(string(body), "land_invalid") {
		t.Fatalf("a range of 1001 commits: status = %d %s, want 400 land_invalid", code, body)
	}
	if code, body := p.do("POST", "/v1/lands", h.landToken(h.repo), h.landBody("feature/small", nil), nil); code != http.StatusAccepted {
		t.Fatalf("a small change: status = %d %s", code, body)
	}
}

func TestLandGrantsNameOnlyTheRefThatTheCallMoves(t *testing.T) {
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true, proxied: true})
	base := h.seedMain()
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	h.protectMain()
	h.awaitLand(p, h.postLand(p, "", "feature/x", map[string]any{"delete_branch": true}).ID, "landed")

	h.mu.Lock()
	seen := slices.Clone(h.grants)
	h.mu.Unlock()
	var landToken, deleteToken string
	for _, g := range seen {
		c := g.claims(t)
		moves := g.method == http.MethodPost && (strings.HasSuffix(g.path, "/lands") || strings.HasSuffix(g.path, "/lands:trial"))
		del := g.method == http.MethodDelete && strings.Contains(g.path, "/refs/")
		switch {
		case moves && !slices.Equal(c.Refs, []string{"main"}), del && !slices.Equal(c.Refs, []string{"feature/x"}):
			t.Errorf("%s %s: grant refs = %q", g.method, g.path, c.Refs)
		case !moves && !del && len(c.Refs) != 0:
			t.Errorf("%s %s: a grant that moves no ref names refs %q", g.method, g.path, c.Refs)
		}
		if moves && strings.HasSuffix(g.path, "/lands") {
			landToken = g.token
		}
		if del {
			deleteToken = g.token
		}
	}
	if landToken == "" || deleteToken == "" {
		t.Fatalf("saw no land call or no delete: %v", seen)
	}
	h.branchFrom(h.mainTip(), "feature/y", "d.txt", "d\n")
	y, err := h.readClient().Ref(t.Context(), "feature/y")
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"land grant": landToken, "delete grant": deleteToken} {
		moved := h.motte.Client(tok)
		if _, err := moved.SwapRef(t.Context(), "feature/y", y.Generation, base); err == nil {
			t.Errorf("the %s of ward moved feature/y", name)
		}
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

	h.fetchAll()
	code, out, errOut := run("-C", h.work, h.repo, "feature/clean")
	main, _ := h.readClient().Ref(t.Context(), "main")
	if code != 0 || errOut != "" || !strings.Contains(out, "tip "+main.Commit+"\n") || !strings.Contains(out, clean+" -> "+main.Commit+"\n") {
		t.Fatalf("clean land: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if main.Commit == moved {
		t.Fatal("the land did not move main")
	}

	code, out, errOut = run("-C", h.work, h.repo, "feature/bad")
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
	if code, _, errOut := run("-C", h.work, h.repo, "feature/bad"); code != 2 || !slices.Contains(strings.Fields(errOut), "WARD_TOKEN,") {
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

func TestLandRepeatsTheSameCallWhenMotteAnswersWithATimeoutAfterItLanded(t *testing.T) {
	h := newLandHarness(t)
	clk := newFakeClock(func() { h.note("timer", "") })
	p := h.startWard(wardOpts{noMirror: true, proxied: true, clock: clk})
	base := h.seedMain()
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	h.landTimeouts.Store(8)
	l := h.postLand(p, "main", "feature/x", nil)
	for range 8 {
		h.waitFor("the pause before the repeat", func() bool { return clk.hasTimer(2 * time.Second) })
		clk.advance(2 * time.Second)
	}
	h.awaitLand(p, l.ID, "landed")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(slices.Compact(slices.Clone(h.landBodies))) != 1 || !strings.Contains(h.landBodies[0], `"id":"`+l.ID+`-1"`) {
		t.Fatalf("land calls = %q: every repeat must carry the same id and values", h.landBodies)
	}
}

// coldLand sets up a land that needs the merge base walk, and delays the first object reads of motte.
func (h *harness) coldLand(budget time.Duration, reads int64, delay time.Duration) (*wardProc, landState) {
	h.t.Helper()
	p := h.startWard(wardOpts{noMirror: true, proxied: true, readBudget: budget})
	base := h.seedMain()
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	h.branchFrom(base, "main", "b.txt", "b\n")
	h.protectMain()
	h.slowFor.Store(int64(delay))
	h.slowLeft.Store(reads)
	return p, h.postLand(p, "main", "feature/x", nil)
}

func TestLandSucceedsWhenOneReadOfMotteIsSlowButWithinItsBudget(t *testing.T) {
	h := newLandHarness(t)
	p, l := h.coldLand(time.Second, 1, 300*time.Millisecond)
	if got := h.awaitLand(p, l.ID, "landed"); got.Error != nil {
		t.Fatalf("land = %+v", got)
	}
}

func TestLandFailsWithLandTimeoutWhenMotteReadsNeverEndInTime(t *testing.T) {
	h := newLandHarness(t)
	p, l := h.coldLand(40*time.Millisecond, 1000, -1)
	got := h.awaitLand(p, l.ID, "failed")
	if got.Error == nil || got.Error.Code != "land_timeout" {
		t.Fatalf("land = %+v, want the error land_timeout", got)
	}
}

func TestLandAcceptsAnOIDCIdentityOnItsRepositoryOnly(t *testing.T) {
	h := newLandHarness(t)
	iss := identitytest.NewIssuer(t, "k1")
	p := h.startWard(wardOpts{noMirror: true, oidc: iss})
	base := h.seedMain()
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	jwt := func(email string) string {
		return iss.Sign(t, map[string]any{"iss": "https://idp.example.test", "aud": "aud1", "email": email, "email_verified": true, "exp": time.Now().Add(time.Hour).Unix()})
	}
	dev := jwt("dev@dev.test")
	if code, body := p.do("POST", "/v1/lands", jwt("dev@other.test"), h.landBody("feature/x", nil), nil); code != http.StatusUnauthorized {
		t.Fatalf("an identity that no rule lists: status = %d %s", code, body)
	}
	if code, body := p.do("POST", "/v1/lands", dev, h.landBody("feature/x", map[string]any{"repo": "native:other"}), nil); code != http.StatusUnauthorized {
		t.Fatalf("a repository that no rule lists: status = %d %s", code, body)
	}
	code, body := p.do("POST", "/v1/lands", dev, h.landBody("feature/x", nil), nil)
	var l landState
	if code != http.StatusAccepted || json.Unmarshal(body, &l) != nil || l.ID == "" {
		t.Fatalf("land by an OIDC identity = %d %s", code, body)
	}
	for path, want := range map[string]int{"/v1/lands/" + l.ID: http.StatusOK, "/v1/lands/nope": http.StatusUnauthorized, "/v1/mirrors": http.StatusUnauthorized, "/v1/tokens": http.StatusUnauthorized} {
		if code, _ := p.do("GET", path, dev, nil, nil); code != want {
			t.Errorf("GET %s by an OIDC identity: status = %d, want %d", path, code, want)
		}
	}
}

func TestLandSendsTheAccessTokenInTheHeaderOfAProxyAndNoBearer(t *testing.T) {
	t.Parallel()
	iss := identitytest.NewIssuer(t, "k1")
	jwt := iss.Sign(t, map[string]any{"iss": "https://idp.example.test", "aud": "aud1", "email": "dev@dev.test", "email_verified": true, "exp": time.Now().Add(time.Hour).Unix()})
	for header, env := range map[string]map[string]string{"cf-access-token": {}, "X-Edge-Jwt": {"WARD_ACCESS_HEADER": "X-Edge-Jwt"}} {
		t.Run(header, func(t *testing.T) {
			h := newLandHarness(t)
			p := h.startWard(wardOpts{noMirror: true, oidc: iss, oidcHeader: header})
			base := h.seedMain()
			h.branchFrom(base, "feature/x", "c.txt", "c\n")
			env["WARD_URL"], env["WARD_ACCESS_TOKEN"] = p.URL, jwt
			h.fetchAll()
			var out, errOut bytes.Buffer
			if code := runLand(t.Context(), []string{"-C", h.work, h.repo, "feature/x"}, func(k string) string { return env[k] }, time.Now, &out, &errOut); code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out.String(), errOut.String())
			}
		})
	}
}

func TestOperatorIssuesAUserTokenThatLandsUntilRevoked(t *testing.T) {
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true})
	base := h.seedMain()
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	op := h.token(token.ScopeOperator, "")
	create := func(tok string, body map[string]any) (int, []byte) {
		raw, _ := json.Marshal(body)
		return p.do("POST", "/v1/tokens", tok, raw, nil)
	}
	grant := map[string]any{"principal": "dev@example.test", "scope": "land", "repos": []string{h.repo}}
	for name, tok := range map[string]string{"none": "", "land": h.landToken(h.repo)} {
		if code, _ := create(tok, grant); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Errorf("issue with %s token: status = %d, want a refusal", name, code)
		}
	}
	if code, _ := create(op, map[string]any{"principal": "dev@example.test", "scope": "operator", "repos": []string{h.repo}}); code != http.StatusBadRequest {
		t.Errorf("a token with the scope operator: status = %d, want 400", code)
	}
	code, body := create(op, grant)
	var made struct {
		Token     string              `json:"token"`
		UserToken struct{ ID string } `json:"user_token"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &made) != nil || made.Token == "" {
		t.Fatalf("issue = %d %s", code, body)
	}
	if code, _ := create(made.Token, grant); code != http.StatusUnauthorized {
		t.Errorf("issue with a user token: status = %d, want 401", code)
	}
	if code, _ := p.do("POST", "/v1/lands", made.Token, h.landBody("feature/x", map[string]any{"repo": "native:other"}), nil); code != http.StatusUnauthorized {
		t.Errorf("land on an unlisted repository: status = %d, want 401", code)
	}
	if code, body := p.do("POST", "/v1/lands", made.Token, h.landBody("feature/x", nil), nil); code != http.StatusAccepted {
		t.Fatalf("land by a user token = %d %s", code, body)
	}
	if code, body := p.do("GET", "/v1/tokens", op, nil, nil); code != http.StatusOK || !strings.Contains(string(body), made.UserToken.ID) || strings.Contains(string(body), made.Token) {
		t.Errorf("list = %d %s, want the id and never the token", code, body)
	}
	if code, _ := p.do("DELETE", "/v1/tokens/"+made.UserToken.ID, op, nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: status = %d", code)
	}
	if code, _ := p.do("POST", "/v1/lands", made.Token, h.landBody("feature/x", nil), nil); code != http.StatusUnauthorized {
		t.Errorf("land by a revoked token: status = %d, want 401", code)
	}
	env := map[string]string{"WARD_URL": p.URL, "WARD_TOKEN": op}
	var out, errOut bytes.Buffer
	if code := runToken(t.Context(), []string{"create", "--principal", "dev@example.test", "--repo", h.repo}, func(k string) string { return env[k] }, time.Now, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "wdu_") {
		t.Errorf("ward token create: exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

func TestLandRefusesARefThatNoPatternNamesAndAnswersARepeatOfAnID(t *testing.T) {
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true})
	base := h.seedChecked(`{"checks":[]}`)
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	h.branchFrom(base, "feature/y", "d.txt", "d\n")
	post := func(extra map[string]any) (int, landState, string) {
		extra["base"] = base
		code, body := p.do("POST", "/v1/lands", h.landToken(h.repo), h.landBody("feature/x", extra), nil)
		var l landState
		_ = json.Unmarshal(body, &l)
		return code, l, string(body)
	}
	if code, _, body := post(map[string]any{"ref": "release/1"}); code != http.StatusForbidden || !strings.Contains(body, "ref_not_allowed") {
		t.Fatalf("a ref that no pattern names: %d %s", code, body)
	}
	first := h.postLand(p, "main", "feature/x", map[string]any{"id": "retry-0001"})
	if first.ID != "retry-0001" {
		t.Fatalf("land id = %q, want the id of the caller", first.ID)
	}
	if code, again, body := post(map[string]any{"ref": "main", "id": "retry-0001"}); code != http.StatusAccepted || again.ID != first.ID {
		t.Fatalf("a repeat of the id: %d %s", code, body)
	}
	h.awaitLand(p, first.ID, "landed")
	if code, _, body := post(map[string]any{"ref": "other", "id": "retry-0001"}); code != http.StatusConflict || !strings.Contains(body, "land_id_in_use") {
		t.Fatalf("the id with another ref: %d %s", code, body)
	}
	for _, id := range []string{"bad id", "short", "release.1", "-leading-dash", strings.Repeat("x", 49)} {
		if code, _, body := post(map[string]any{"id": id}); code != http.StatusBadRequest {
			t.Fatalf("the id %q: %d %s", id, code, body)
		}
	}
	edge := h.postLand(p, "main", "feature/y", map[string]any{"id": "A" + strings.Repeat("_-", 23) + "z"})
	h.awaitLand(p, edge.ID, "landed")
}
