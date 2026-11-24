package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/token"
)

const (
	runnerSecret = "runner-secret-0123456789"
	runnerEnv    = "env1"
	unitChecks   = `{"checks":[{"name":"unit","run":["make","test"],"timeout_seconds":60}]}`
)

type runnerRequest struct {
	RequestID string `json:"request_id"`
	Repo      string `json:"repo"`
	Tree      string `json:"tree"`
	Commit    string `json:"commit"`
	Check     struct {
		Name           string   `json:"name"`
		Run            []string `json:"run"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	} `json:"check"`
	CheckHash string `json:"check_hash"`
	Env       string `json:"env"`
}

// fakeRunner is the runner of a test. It checks the signature of each request and hands the request to the test.
type fakeRunner struct {
	t      *testing.T
	srv    *httptest.Server
	reqs   chan runnerRequest
	status atomic.Int32
	bad    atomic.Int32
}

func newRunner(t *testing.T) *fakeRunner {
	t.Helper()
	r := &fakeRunner{t: t, reqs: make(chan runnerRequest, 64)}
	r.status.Store(http.StatusOK)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		mac := hmac.New(sha256.New, []byte(runnerSecret))
		mac.Write(body)
		if req.Header.Get("Ward-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			r.bad.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var rr runnerRequest
		if json.Unmarshal(body, &rr) != nil {
			r.bad.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		code := int(r.status.Load())
		r.reqs <- rr
		w.WriteHeader(code)
	}))
	t.Cleanup(func() {
		r.srv.Close()
		if n := r.bad.Load(); n != 0 {
			t.Errorf("%d runner requests had a bad signature or body", n)
		}
	})
	return r
}

// next returns the next request that the runner got.
func (r *fakeRunner) next() runnerRequest {
	r.t.Helper()
	select {
	case rr := <-r.reqs:
		return rr
	case <-time.After(20 * time.Second): //lint:allow wallclock a wait on a request from another goroutine that no bubble holds
		r.t.Fatal("the runner got no request")
		return runnerRequest{}
	}
}

func (h *harness) checkToken(repo string) string { return h.token(token.ScopeCheck, repo) }

func (p *wardProc) result(tok string, body map[string]any) (int, []byte) {
	p.h.t.Helper()
	raw, _ := json.Marshal(body)
	return p.do("POST", "/v1/results", tok, raw, nil)
}

// answer posts a result for a request with the check token of the repository and expects 200.
func (p *wardProc) answer(req runnerRequest, status, summary string) {
	p.h.t.Helper()
	code, body := p.result(p.h.checkToken(p.h.repo), map[string]any{"request_id": req.RequestID, "status": status, "summary": summary, "log_url": "https://logs.example/" + req.RequestID})
	if code != http.StatusOK {
		p.h.t.Fatalf("result = %d %s", code, body)
	}
}

type landState struct {
	ID, Repo, Ref, Branch, State, Tree string
	Checks                             []struct{ Name, State string }
	Tip                                string
	Generation                         int64
	FastForward                        bool `json:"fast_forward"`
	Commits                            []struct{ Old, New string }
	BranchDeleted                      bool `json:"branch_deleted"`
	Error                              *struct {
		Code, Message string
		Details       map[string]any
	}
}

func (p *wardProc) landGet(tok, id, query string) (int, landState) {
	p.h.t.Helper()
	code, body := p.do("GET", "/v1/lands/"+id+query, tok, nil, nil)
	var l landState
	_ = json.Unmarshal(body, &l)
	return code, l
}

// postLand posts a land with the land token of the repository and expects 202.
func (h *harness) postLand(p *wardProc, ref, branch string, extra map[string]any) landState {
	h.t.Helper()
	m := map[string]any{"ref": ref}
	for k, v := range extra {
		m[k] = v
	}
	code, body := p.do("POST", "/v1/lands", h.landToken(h.repo), h.landBody(branch, m), nil)
	var l landState
	if code != http.StatusAccepted || json.Unmarshal(body, &l) != nil || l.ID == "" || l.State != "queued" {
		h.t.Fatalf("post land = %d %s", code, body)
	}
	return l
}

// awaitLand reads the land until it is in state want. Each wait names the state that the last read saw.
func (h *harness) awaitLand(p *wardProc, id, want string) landState {
	h.t.Helper()
	for query := ""; ; {
		code, l := p.landGet(h.landToken(h.repo), id, query)
		switch {
		case code != http.StatusOK:
			h.t.Fatalf("read land = %d", code)
		case l.State == want:
			return l
		case l.State == "failed" || l.State == "landed":
			h.t.Fatalf("land ended in %s (error %+v), want %s", l.State, l.Error, want)
		}
		query = "?wait=30&state=" + l.State
	}
}

// seedChecked puts the check file and a.txt on main in one commit and returns main.
func (h *harness) seedChecked(file string) string {
	h.t.Helper()
	base := h.commitOn(h.mainTip(), map[string]string{"a.txt": "base\n", ".ward/checks.json": file}, "seed")
	h.push("main", base, false)
	h.protectMain()
	return base
}

func (h *harness) treeOf(commit string) string {
	h.t.Helper()
	c, err := h.readClient().Commit(h.t.Context(), commit)
	if err != nil {
		h.t.Fatal(err)
	}
	return c.Tree
}

func (h *harness) mainTip() string {
	h.t.Helper()
	r, err := h.readClient().Ref(h.t.Context(), "main")
	if err != nil {
		h.t.Fatal(err)
	}
	return r.Commit
}

// signalWriter collects output and closes seen when it holds the text.
type signalWriter struct {
	mu   sync.Mutex
	sb   strings.Builder
	want string
	seen chan struct{}
	once sync.Once
}

func (w *signalWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sb.Write(b)
	if strings.Contains(w.sb.String(), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(b), nil
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sb.String()
}

// newTickClock returns a fake clock and a function that waits until a timer of d is set on it.
func newTickClock(t *testing.T) (*fakeClock, func(time.Duration)) {
	ticks := make(chan struct{}, 64)
	clk := newFakeClock(func() { ticks <- struct{}{} })
	return clk, func(d time.Duration) {
		t.Helper()
		for !clk.hasTimer(d) {
			select {
			case <-ticks:
			case <-time.After(20 * time.Second): //lint:allow wallclock a wait on a goroutine that no bubble holds
				t.Fatalf("no timer of %s", d)
			}
		}
	}
}
