package runner_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/runner"
	"github.com/majorcontext/ward/internal/runner/sandbox/fake"
	"github.com/majorcontext/ward/internal/token"
	"github.com/majorcontext/ward/runnerproto"
)

// tickClock fires every timer when the test sends on tick.
type tickClock struct{ tick chan time.Time }

func (tickClock) Now() time.Time                         { return time.Now() }
func (c tickClock) After(time.Duration) <-chan time.Time { return c.tick }

// pullWard is a ward that leases one request, answers each later poll when its request ends, and records what it gets.
type pullWard struct {
	polls, renewals chan string
	results         chan token.Claims
	renewStatus     int
}

func newPullWard(t *testing.T, pub ed25519.PublicKey, argv ...string) (*pullWard, string) {
	t.Helper()
	w := &pullWard{polls: make(chan string, 8), renewals: make(chan string, 8), results: make(chan token.Claims, 8), renewStatus: http.StatusOK}
	v := token.NewVerifier(token.Key{Kid: "k", Public: pub, Scopes: []token.Scope{token.ScopeRunner, token.ScopeCheck}})
	first := true
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+runnerproto.LeasesPath, func(rw http.ResponseWriter, r *http.Request) {
		c, err := v.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), time.Now())
		var in runnerproto.LeaseRequest
		if err != nil || c.Scope != token.ScopeRunner || json.NewDecoder(r.Body).Decode(&in) != nil {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.polls <- string(in.Isolation)
		if !first {
			<-r.Context().Done()
			return
		}
		first = false
		chk := runnerproto.Check{Name: "unit", Run: argv, TimeoutSeconds: 300}
		_ = json.NewEncoder(rw).Encode(runnerproto.Lease{LeaseID: "l1", TTLSeconds: 3, Request: runnerproto.Request{
			RequestID: "r1", Repo: "native:widgets", Tree: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40), Check: chk, CheckHash: chk.Hash(), Env: "e"}})
	})
	mux.HandleFunc("POST "+runnerproto.RenewPath("l1"), func(rw http.ResponseWriter, _ *http.Request) {
		w.renewals <- "l1"
		rw.WriteHeader(w.renewStatus)
		_, _ = rw.Write([]byte(`{"ttl_seconds":3}`))
	})
	mux.HandleFunc("POST "+runnerproto.ResultsPath, func(rw http.ResponseWriter, r *http.Request) {
		c, err := v.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), time.Now())
		if err != nil {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.results <- c
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return w, srv.URL
}

func startPull(t *testing.T, wardURL string, key ed25519.PrivateKey, clk tickClock) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := runner.New(ctx, runner.Options{
		WardURL:     wardURL,
		RunnerToken: func(exp time.Time) (string, error) { return runnerproto.MintRunnerToken("k", key, "test", exp) },
		Token: func(repo string, exp time.Time) (string, error) {
			return runnerproto.MintCheckToken("k", key, repo, "test", exp)
		},
		Fetch:       func(context.Context, runnerproto.Request, string) error { return nil },
		Sandbox:     fake.Sandbox{Level: runnerproto.IsolationGVisor},
		Concurrency: 1,
		WorkDir:     t.TempDir(),
		Clock:       clk,
	})
	done := make(chan struct{})
	go func() { r.Pull(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; r.Wait() })
}

func TestPullRunsALeasedCheckAndPostsTheResult(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	w, url := newPullWard(t, pub, "sh", "-c", "exit 0")
	startPull(t, url, key, tickClock{make(chan time.Time)})
	if iso := <-w.polls; iso != "gvisor" {
		t.Errorf("the poll reported isolation %q", iso)
	}
	if c := <-w.results; c.Scope != token.ScopeCheck || c.Repo != "native:widgets" || c.Kid != "k" {
		t.Errorf("result token claims = %+v", c)
	}
}

func TestPullStopsACheckWhenTheLeaseIsLost(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	w, url := newPullWard(t, pub, "sh", "-c", "sleep 300")
	w.renewStatus = http.StatusGone
	clk := tickClock{make(chan time.Time)}
	startPull(t, url, key, clk)
	<-w.polls
	clk.tick <- time.Now()
	<-w.renewals
	<-w.polls
	select {
	case c := <-w.results:
		t.Fatalf("a check that lost its lease posted a result: %+v", c)
	default:
	}
}
