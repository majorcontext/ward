package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/motte/grant"
	"github.com/majorcontext/motte/testserver"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/config"
	"github.com/majorcontext/ward/internal/identity/identitytest"
	"github.com/majorcontext/ward/internal/mirror"
	"github.com/majorcontext/ward/internal/token"
)

const (
	mirrorName = "m1"
	remoteUser = "ward-bot"
	remotePass = "s3cret"
)

// gitIn runs git in dir with a clean environment and returns its trimmed output.
func gitIn(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// remote is a bare repository served by the test process behind HTTP Basic.
type remote struct {
	t    testing.TB
	dir  string
	srv  *httptest.Server
	URL  string
	pass atomicString
	user string
	gate atomic.Pointer[gate]

	mu      sync.Mutex
	pushes  []string
	refuse  map[string]bool
	orphans int
	logging bool
}

// gate holds the ls-remote requests of the remote until release is closed.
type gate struct {
	entered chan struct{}
	release chan struct{}
}

// hold makes the next ref listing of the remote wait. It returns the channel that reports the wait and the release.
func (r *remote) hold() (<-chan struct{}, func()) {
	g := &gate{entered: make(chan struct{}, 8), release: make(chan struct{})}
	r.gate.Store(g)
	return g.entered, func() { r.gate.Store(nil); close(g.release) }
}

type atomicString struct {
	mu sync.Mutex
	s  string
}

func (a *atomicString) set(s string) { a.mu.Lock(); a.s = s; a.mu.Unlock() }
func (a *atomicString) get() string  { a.mu.Lock(); defer a.mu.Unlock(); return a.s }

func newRemote(t *testing.T) *remote {
	t.Helper()
	root := t.TempDir()
	r := &remote{t: t, dir: filepath.Join(root, "r.git"), user: remoteUser}
	if err := os.CopyFS(r.dir, os.DirFS(bareTemplate)); err != nil {
		t.Fatal(err)
	}
	r.pass.set(remotePass)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		u, p, ok := req.BasicAuth()
		if !ok || u != r.user || p != r.pass.get() {
			w.Header().Set("WWW-Authenticate", `Basic realm="r"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if g := r.gate.Load(); g != nil && strings.HasSuffix(req.URL.Path, "/info/refs") {
			g.entered <- struct{}{}
			<-g.release
		}
		r.serveGit(w, req)
	}))
	t.Cleanup(r.srv.Close)
	r.URL = r.srv.URL + "/r.git"
	return r
}

// tip reads the loose ref. It spawns git only when the remote packed its refs, which a test remote never does by itself.
func (r *remote) tip(ref string) string {
	if sha, err := os.ReadFile(filepath.Join(r.dir, ref)); err == nil {
		if s := strings.TrimSpace(string(sha)); !strings.HasPrefix(s, "ref:") {
			return s
		}
	} else if _, perr := os.Stat(filepath.Join(r.dir, "packed-refs")); perr != nil {
		return ""
	}
	cmd := exec.Command("git", "--git-dir", r.dir, "rev-parse", "--verify", "-q", ref)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func (r *remote) setRef(ref, commit string) {
	if err := writeRef(r.dir, ref, commit); err != nil {
		r.t.Fatal(err)
	}
}

// orphan makes a commit that no motte ref holds.
func (r *remote) orphan() string {
	empty := newObj("tree", nil)
	r.mu.Lock()
	r.orphans++
	n := r.orphans
	r.mu.Unlock()
	c := newObj("commit", fmt.Appendf(nil, "tree %s\nauthor a <a@example.com> %[2]d +0000\ncommitter a <a@example.com> %[2]d +0000\n\noutside\n", empty.id, n))
	if err := writeLoose(r.dir, []gitObj{empty, c}); err != nil {
		r.t.Fatal(err)
	}
	return c.id
}

func (r *remote) deleteRef(ref string) {
	if err := os.Remove(filepath.Join(r.dir, ref)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *remote) writeHook(script string) {
	if err := os.WriteFile(filepath.Join(r.dir, "hooks", "pre-receive"), []byte(script), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

// rejectRef makes the remote refuse every push that names ref. A file remote runs the hook, and the HTTP server of the
// remote checks the same set.
func (r *remote) rejectRef(ref string) {
	r.writeHook(fmt.Sprintf("#!/bin/sh\nwhile read old new name; do [ \"$name\" = %q ] && { echo refused >&2; exit 1; }; done\nexit 0\n", ref))
	r.mu.Lock()
	r.refuse = map[string]bool{ref: true}
	r.mu.Unlock()
}

// logPushes makes the remote record each ref update. pushLog returns the record.
func (r *remote) logPushes() {
	r.writeHook(fmt.Sprintf("#!/bin/sh\ncat >> %q\n", filepath.Join(r.dir, "pushes.log")))
	r.mu.Lock()
	r.logging = true
	r.mu.Unlock()
}

func (r *remote) pushLog() string {
	out, _ := os.ReadFile(filepath.Join(r.dir, "pushes.log"))
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(out) + strings.Join(r.pushes, "")
}

func (r *remote) allowAll() {
	_ = os.Remove(filepath.Join(r.dir, "hooks", "pre-receive"))
	r.mu.Lock()
	r.refuse = nil
	r.mu.Unlock()
}

type harness struct {
	t        *testing.T
	motte    *testserver.Server
	repo     string
	repoPath string
	work     string
	remote   *remote
	dataDir  string
	files    string
	addr     string
	secret   string
	notes    chan string
	cursor   string
	callerSK ed25519.PrivateKey
	callerPK ed25519.PublicKey
	hist     history

	// http makes the mirror reach the remote over HTTP, which a test needs for the transport, the credential or a held request.
	// Any other test reaches the remote as a file URL, which starts no helper for HTTP.
	http bool

	started    bool
	ln         *net.TCPListener
	motteProxy *httptest.Server
	motteGate  atomic.Pointer[gate]
	// limitPushes makes the proxy answer 503 to a push of git once pushesLeft pushes passed.
	// holdLand makes the proxy answer no land of main: it forwards the land, and keeps the reply until the caller leaves.
	holdLand     atomic.Bool
	landTimeouts atomic.Int64
	// slowLeft object reads of motte wait slowFor in the proxy. A negative slowFor waits until the caller leaves.
	slowLeft atomic.Int64
	slowFor  atomic.Int64
	// logHook sees each log line of ward.
	logHook     atomic.Pointer[func(string)]
	landRecord  chan struct{}
	landBodies  []string
	grants      []seenGrant
	limitPushes atomic.Bool
	pushesLeft  atomic.Int64
	mu          sync.Mutex
	counts      map[string]int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return buildHarness(t, testserver.StartLocal(&seededTB{TB: t}), newRemote(t))
}

// newLandHarness is a harness for a test that needs no mirror. It shares the one motte of the package and makes no remote.
// Each test has a repository of its own on that motte, and no such test reads the events of motte or its subscriptions.
func newLandHarness(t *testing.T) *harness {
	t.Helper()
	return buildHarness(t, sharedMotte, nil)
}

func buildHarness(t *testing.T, motte *testserver.Server, rem *remote) *harness {
	t.Helper()
	t.Parallel()
	h := &harness{t: t, motte: motte, remote: rem, dataDir: t.TempDir(), files: t.TempDir(),
		secret: strings.Repeat("h", 32), notes: make(chan string, 4096), counts: map[string]int{}, landRecord: make(chan struct{}, 4)}
	h.callerPK, h.callerSK, _ = ed25519.GenerateKey(rand.Reader)
	if err := os.WriteFile(filepath.Join(h.dataDir, "state.db"), stateTemplate, 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := h.motte.Client(h.motte.OperatorGrant(t)).CreateRepo(t.Context(), "widgets", "o1")
	if err != nil {
		t.Fatal(err)
	}
	h.repoPath, h.repo = path, "native:"+path
	h.work = t.TempDir()
	u := strings.Replace(h.motte.URL, "http://", "http://git:"+h.motte.WriteGrant(t, h.repo)+"@", 1) + "/git/native/" + path + ".git"
	if err := os.CopyFS(h.work, os.DirFS(workTemplate)); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.OpenFile(filepath.Join(h.work, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(cfg, "[remote \"origin\"]\n\turl = %s\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n", u)
	if err != nil || cfg.Close() != nil {
		t.Fatal("write the remote of the working clone")
	}
	return h
}

func (h *harness) note(kind, _ string) {
	h.mu.Lock()
	h.counts[kind]++
	h.mu.Unlock()
	select {
	case h.notes <- kind:
	default:
	}
}

func (h *harness) count(kind string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[kind]
}

// waitFor returns when cond holds. It re-checks after each unit of work that ward reports.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	timeout := time.After(20 * time.Second) //lint:allow wallclock a wait on another process that no bubble holds
	for !cond() {
		select {
		case <-h.notes:
		case <-timeout:
			h.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (h *harness) git(args ...string) string { return gitIn(h.t, h.work, args...) }

// gitCommit adds a commit to the working clone and returns it.
func (h *harness) gitCommit(file, content string) string {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(h.work, file)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.work, file), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.git("add", file)
	h.git("commit", "-q", "-m", "edit "+file)
	return h.head()
}

// head returns the commit of the current branch of the working clone. It reads the loose ref, because git has no
// reason to pack the refs of a clone that lives for one test, and a spawn of git costs more than the read.
func (h *harness) head() string {
	h.t.Helper()
	ref, err := os.ReadFile(filepath.Join(h.work, ".git", "HEAD"))
	if name, ok := strings.CutPrefix(strings.TrimSpace(string(ref)), "ref: "); err == nil && ok {
		if sha, err := os.ReadFile(filepath.Join(h.work, ".git", name)); err == nil {
			return strings.TrimSpace(string(sha))
		}
	}
	return h.git("rev-parse", "HEAD")
}

// push writes a motte ref: force sets the leading plus of the refspec. An empty commit deletes.
func (h *harness) push(ref, commit string, force bool) {
	h.t.Helper()
	if _, ok := h.hist.files[commit]; ok {
		h.swap(ref, commit)
		return
	}
	plus := ""
	if force {
		plus = "+"
	}
	h.git("push", "-q", "origin", plus+commit+":refs/heads/"+ref)
}

func (h *harness) deleteRef(ref string) {
	h.t.Helper()
	mc := h.motte.Client(h.motte.MergeGrant(h.t, h.repo))
	cur, err := mc.Ref(h.t.Context(), ref)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := mc.DeleteRef(h.t.Context(), ref, cur.Generation); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) readClient() *client.Client { return h.motte.Client(h.motte.ReadGrant(h.t, h.repo)) }

func (h *harness) operatorClient() *client.Client { return h.motte.Client(h.motte.OperatorGrant(h.t)) }

func (h *harness) token(scope token.Scope, repo string) string { return h.tokenAs("c", scope, repo) }

func (h *harness) tokenAs(kid string, scope token.Scope, repo string) string {
	tok, err := token.Signer{Kid: kid, Key: h.callerSK}.Mint(token.Claims{Scope: scope, Repo: repo, Exp: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

type wardOpts struct {
	// required is the WARD_REQUIRED_CHECKS text with %s for the repository, or empty.
	required string
	refs     []string
	prune    bool
	budget   string
	ttl      string
	clock    mirror.Clock
	// relative gives ward a data directory and a credential file under the working directory of the test, as relative paths.
	relative bool
	// proxied sends ward to motte through a proxy that holdMotte can hold.
	proxied bool
	noWait  bool
	// app makes the mirror take its credential from a GitHub App installation at this fake.
	app *fakeGitHub
	// seed sets seed on the mirror. seedObjects lowers the object limit of one seed push.
	seed        bool
	seedObjects string
	// seedInflated lowers the inflated byte limit of one seed push.
	seedInflated string
	// runner is the runner of the repository of the harness.
	runner *fakeRunner
	// pullKids makes the repository of the harness a pull repository that these runner kids serve, at pullMin isolation.
	pullKids []string
	pullMin  string
	// noMirror starts ward with no mirror, and so with no remote work and no wait for a reconcile.
	noMirror bool
	// readBudget sets the time limit of a read of motte by a land.
	readBudget time.Duration
	// oidc turns on OIDC identities, signed by this issuer, for the users of the domain dev.test on the repository of the harness.
	oidc *identitytest.Issuer
	// oidcHeader is the header that carries the JWT. Empty means Authorization.
	oidcHeader string
}

type wardProc struct {
	h    *harness
	URL  string
	stop func()
}

func (h *harness) writeFile(name, content string) string {
	p := filepath.Join(h.files, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// startWard runs ward in the process, on the data directory of the harness. It returns when ward has registered its
// subscription and, on the first start, has reconciled the mirror.
func (h *harness) startWard(o wardOpts) *wardProc {
	h.t.Helper()
	if o.refs == nil {
		o.refs = []string{"feature/*"}
	}
	seed := base64.StdEncoding.EncodeToString(h.motte.Operator.Key.Seed())
	var keys []map[string]any
	for _, kid := range []string{"c", "c2", "c3"} {
		keys = append(keys, map[string]any{"kid": kid, "key": base64.StdEncoding.EncodeToString(h.callerPK), "scopes": []string{"read", "operator", "land", "check", "runner"}})
	}
	trusted, _ := json.Marshal(keys)
	dataDir, credFile := h.dataDir, h.credFile()
	if o.relative {
		dir, err := os.MkdirTemp(".", "rel-")
		if err != nil {
			h.t.Fatal(err)
		}
		h.t.Cleanup(func() { _ = os.RemoveAll(dir) })
		dataDir = filepath.Join(dir, "data")
		credFile = filepath.Join(dir, "cred")
		if err := os.WriteFile(credFile, []byte(remotePass+"\n"), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	entry := mirror.Entry{Name: mirrorName, Repo: h.repo, Remote: "https://remote.invalid/r.git", Refs: o.refs,
		Username: remoteUser, CredentialFile: credFile, PruneUnknown: o.prune, Seed: o.seed}
	githubAPI := ""
	if o.app != nil {
		entry.Username, entry.CredentialFile = "", ""
		entry.GitHubApp = &mirror.GitHubApp{AppID: appID, InstallationID: installationID, KeyFile: o.app.keyFile}
		githubAPI = o.app.srv.URL
	}
	entries, _ := json.Marshal([]mirror.Entry{entry})
	if o.noMirror {
		entries = nil
	}
	runners := ""
	if o.runner != nil {
		raw, _ := json.Marshal([]check.Runner{{Repo: h.repo, Kids: []string{runnerKid}, Env: runnerEnv}})
		runners = string(raw)
	}
	if o.pullKids != nil {
		raw, _ := json.Marshal([]check.Runner{{Repo: h.repo, Kids: o.pullKids, MinIsolation: o.pullMin, Env: runnerEnv}})
		runners = string(raw)
	}
	motteURL := h.motte.URL
	if o.proxied {
		motteURL = h.proxy().URL
	}
	ln := h.listen()
	h.addr = ln.Addr().String()
	env := map[string]string{
		"WARD_DATA_DIR": dataDir, "WARD_MOTTE_URL": motteURL, "WARD_MOTTE_KID": "cp",
		"WARD_MOTTE_KEY_FILE": h.writeFile("motte.key", seed), "WARD_PUBLIC_URL": "http://" + h.addr,
		"WARD_HOOK_SECRET_FILE": h.writeFile("hook.secret", h.secret+"\n"), "WARD_TRUSTED_KEYS": string(trusted),
		"WARD_MIRRORS": string(entries), "WARD_RUNNERS": runners, "WARD_GITHUB_API_URL": githubAPI, "WARD_CLONE_DISK_BUDGET": o.budget, "WARD_CLONE_IDLE_TTL": o.ttl,
		"WARD_LAND_REFS": "main,other", "WARD_SEED_MAX_OBJECTS": o.seedObjects, "WARD_SEED_MAX_INFLATED_BYTES": o.seedInflated,
	}
	if o.required != "" {
		env["WARD_REQUIRED_CHECKS"] = strings.ReplaceAll(o.required, "%s", h.repo)
	}
	if o.oidc != nil {
		rules, _ := json.Marshal([]map[string]any{{"principals": []string{"@dev.test"}, "repos": []string{h.repo}}})
		env["WARD_OIDC_ISSUER"], env["WARD_OIDC_AUDIENCE"], env["WARD_OIDC_KEYS_URL"], env["WARD_OIDC_RULES"] = "https://idp.example.test", "aud1", o.oidc.URL, string(rules)
		env["WARD_OIDC_HEADER"] = o.oidcHeader
	}
	adjust := func(c *config.Config) {
		if len(c.Mirror.Mirrors) > 0 {
			c.Mirror.Mirrors[0].Remote = "file://" + h.remote.dir
			if h.http || o.app != nil || o.relative {
				c.Mirror.Mirrors[0].Remote = h.remote.URL
			}
		}
		c.Mirror.Askpass = askpassBin
		c.Land.ReadBudget = o.readBudget
	}
	ctx, cancel := context.WithCancel(h.t.Context())
	done := make(chan error, 1)
	before, reconciles := h.count("init"), h.count("reconcile")
	first := !h.started
	h.started = true
	go func() {
		done <- run(ctx, func(k string) string { return env[k] }, ln, mirror.Options{Clock: o.clock, Done: h.note, Logger: slog.New(slog.NewTextHandler(testLog{h.t, &h.logHook}, nil))}, adjust)
	}()
	p := &wardProc{h: h, URL: "http://" + h.addr}
	var once sync.Once
	p.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				h.t.Errorf("ward stopped with %v", err)
			}
			http.DefaultClient.CloseIdleConnections()
		})
	}
	h.t.Cleanup(p.stop)
	if o.runner != nil {
		o.runner.attach(h, p.URL)
	}
	if o.noWait || o.noMirror {
		return p
	}
	h.waitFor("ward to start", func() bool { return h.count("init") > before })
	if first {
		h.waitFor("the first reconcile", func() bool { return h.count("reconcile") > reconciles })
	}
	return p
}

func (h *harness) credFile() string { return h.writeFile("remote.cred", remotePass+"\n") }

// listen returns a view of the one listener of the harness. ward closes the listener that it serves on, so the view stops
// accepting at Close and leaves the socket open: the port is never released and bound again, and a restart keeps its address.
func (h *harness) listen() net.Listener {
	if h.ln == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			h.t.Fatal(err)
		}
		h.ln = ln.(*net.TCPListener)
		h.t.Cleanup(func() { _ = ln.Close() })
	}
	_ = h.ln.SetDeadline(time.Time{})
	return &keepOpen{TCPListener: h.ln}
}

type keepOpen struct {
	*net.TCPListener
	closed atomic.Bool
}

func (k *keepOpen) Accept() (net.Conn, error) {
	c, err := k.TCPListener.Accept()
	if k.closed.Load() {
		if err == nil {
			_ = c.Close()
		}
		return nil, net.ErrClosed
	}
	return c, err
}

func (k *keepOpen) Close() error {
	k.closed.Store(true)
	return k.TCPListener.SetDeadline(time.Unix(1, 0))
}

func (p *wardProc) do(method, path, tok string, body []byte, hdr map[string]string) (int, []byte) {
	p.h.t.Helper()
	req, err := http.NewRequestWithContext(p.h.t.Context(), method, p.URL+path, bytes.NewReader(body))
	if err != nil {
		p.h.t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

type refView struct {
	Ref, State, Pushed, Failure, Message string
	Attempts                             int
}

type mirrorView struct {
	Name, Repo, State string
	Refs              []refView
}

func (p *wardProc) status() mirrorView {
	p.h.t.Helper()
	code, body := p.do("GET", "/v1/mirrors/"+mirrorName, p.h.token(token.ScopeOperator, ""), nil, nil)
	if code != 200 {
		p.h.t.Fatalf("status = %d %s", code, body)
	}
	var v struct {
		Name, Repo, State string
		Refs              []struct {
			Ref, State, Pushed, Failure, Message string
			Attempts                             int
		}
	}
	if err := json.Unmarshal(body, &v); err != nil {
		p.h.t.Fatal(err)
	}
	out := mirrorView{Name: v.Name, Repo: v.Repo, State: v.State}
	for _, r := range v.Refs {
		out.Refs = append(out.Refs, refView(r))
	}
	return out
}

func (v mirrorView) ref(name string) (refView, bool) {
	for _, r := range v.Refs {
		if r.Ref == name {
			return r, true
		}
	}
	return refView{}, false
}

// metric returns the value of a series of /metrics, such as `ward_hook_events_total{result="repeat"}`.
func (p *wardProc) metric(series string) float64 {
	p.h.t.Helper()
	_, body := p.do("GET", "/metrics", "", nil, nil)
	for line := range strings.Lines(string(body)) {
		if rest, ok := strings.CutPrefix(line, series+" "); ok {
			v, _ := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			return v
		}
	}
	return 0
}

func (h *harness) sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (p *wardProc) deliver(ev api.Event) int {
	p.h.t.Helper()
	body, _ := json.Marshal(ev)
	code, _ := p.do("POST", "/v1/hooks/motte", "", body, map[string]string{api.HeaderSignature: p.h.sign(body),
		api.HeaderPartition: strconv.Itoa(ev.Partition), api.HeaderSequence: strconv.FormatInt(ev.Seq, 10)})
	return code
}

// events reads the events that motte holds after the relay cursor and moves the cursor to the head.
func (h *harness) events() []api.Event {
	h.t.Helper()
	var all []api.Event
	for {
		evs, cur, err := h.operatorClient().Events(h.t.Context(), h.cursor, 1000)
		if err != nil {
			h.t.Fatal(err)
		}
		h.cursor = cur
		if len(evs) == 0 {
			return all
		}
		all = append(all, evs...)
	}
}

// relay delivers every event that motte holds after the relay cursor, as motte would: signed, in order for each partition.
func (h *harness) relay(p *wardProc) []api.Event {
	h.t.Helper()
	evs := h.events()
	for _, ev := range evs {
		if code := p.deliver(ev); code != http.StatusNoContent {
			h.t.Fatalf("delivery of %s seq %d answered %d", ev.Ref, ev.Seq, code)
		}
	}
	return evs
}

// skip moves the relay cursor to the head and delivers nothing: the events are lost to ward.
func (h *harness) skip() []api.Event { return h.events() }

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
	onAdd  func()
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
	d  time.Duration
}

func newFakeClock(onAdd func()) *fakeClock { return &fakeClock{now: time.Now(), onAdd: onAdd} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.Now()
		return ch
	}
	c.mu.Lock()
	c.timers = append(c.timers, fakeTimer{at: c.now.Add(d), ch: ch, d: d})
	c.mu.Unlock()
	c.onAdd()
	return ch
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if t.d != time.Minute { // the sweep of the land queues always waits
			n++
		}
	}
	return n
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	rest := c.timers[:0]
	for _, t := range c.timers {
		if c.now.Before(t.at) {
			rest = append(rest, t)
		} else {
			t.ch <- c.now
		}
	}
	c.timers = rest
}

func gitErr(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	return cmd.Run()
}

type testLog struct {
	t    testing.TB
	hook *atomic.Pointer[func(string)]
}

func (l testLog) Write(b []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(b)))
	if f := l.hook.Load(); f != nil {
		(*f)(string(b))
	}
	return len(b), nil
}

// proxy starts a reverse proxy in front of motte. holdMotte makes it hold the subscription listing.
func (h *harness) proxy() *httptest.Server {
	if h.motteProxy != nil {
		return h.motteProxy
	}
	target, _ := url.Parse(h.motte.URL)
	rp := httputil.NewSingleHostReverseProxy(target)
	h.motteProxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.grants = append(h.grants, seenGrant{r.Method, r.URL.Path, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")})
		h.mu.Unlock()
		if g := h.motteGate.Load(); g != nil && r.URL.Path == "/v1/subscriptions" {
			g.entered <- struct{}{}
			<-g.release
		}
		if strings.Contains(r.URL.Path, "/files/") && h.slowLeft.Add(-1) >= 0 {
			d := time.Duration(h.slowFor.Load())
			if d < 0 {
				<-r.Context().Done()
				return
			}
			select {
			case <-time.After(d): //lint:allow wallclock a delay in a proxy that the client budget measures on the wall clock
			case <-r.Context().Done():
				return
			}
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/lands") {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"ref":"main"`)) {
				h.mu.Lock()
				h.landBodies = append(h.landBodies, string(body))
				h.mu.Unlock()
				if h.landTimeouts.Add(-1) >= 0 {
					rp.ServeHTTP(httptest.NewRecorder(), r)
					w.WriteHeader(http.StatusGatewayTimeout)
					_, _ = io.WriteString(w, `{"error":{"code":"vcs_land_timeout","message":"slow","details":{}}}`)
					return
				}
				if h.holdLand.Load() {
					rp.ServeHTTP(httptest.NewRecorder(), r)
					h.landRecord <- struct{}{}
					<-r.Context().Done()
					return
				}
			}
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") && h.limitPushes.Load() && h.pushesLeft.Add(-1) < 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	h.t.Cleanup(h.motteProxy.Close)
	return h.motteProxy
}

func (h *harness) holdMotte() (<-chan struct{}, func()) {
	h.proxy()
	g := &gate{entered: make(chan struct{}, 8), release: make(chan struct{})}
	h.motteGate.Store(g)
	return g.entered, func() { h.motteGate.Store(nil); close(g.release) }
}

// hasTimer reports whether a timer is waiting for d from the fake now.
func (c *fakeClock) hasTimer(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.timers {
		if t.at.Equal(c.now.Add(d)) {
			return true
		}
	}
	return false
}

// seenGrant is a request that ward sent to motte through the proxy, with its bearer grant.
type seenGrant struct{ method, path, token string }

// claims returns the claims of the grant. It checks no signature.
func (g seenGrant) claims(t testing.TB) grant.Claims {
	t.Helper()
	parts := strings.Split(g.token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[len(parts)-2])
	var c grant.Claims
	if err != nil || json.Unmarshal(raw, &c) != nil {
		t.Fatalf("grant of %s %s is not readable: %v", g.method, g.path, err)
	}
	return c
}
