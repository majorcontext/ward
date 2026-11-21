// Package runnertest is the conformance suite of runnerproto.Runner. It plays ward: it posts signed check requests to the
// runner and holds the results route that checks the check token.
package runnertest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/token"
	"github.com/majorcontext/ward/runnerproto"
)

// Env is what the suite gives the runner under test.
type Env struct {
	Secret  []byte
	WardURL string
	Kid     string
	// Key signs the check tokens of the runner. The ward of the suite trusts its public key for the scope check.
	Key ed25519.PrivateKey
}

// Impl builds the runner under test. Commands use only sh.
type Impl struct {
	New func(t *testing.T, env Env) runnerproto.Runner
}

const (
	repo    = "native:widgets"
	kid     = "runner-test"
	maxWait = 60 * time.Second
)

type posted struct {
	res    runnerproto.Result
	claims token.Claims
}

type ward struct {
	got chan posted
}

func newWard(t *testing.T, pub ed25519.PublicKey) (*ward, string) {
	t.Helper()
	w := &ward{got: make(chan posted, 16)}
	v := token.NewVerifier(token.Key{Kid: kid, Public: pub, Scopes: []token.Scope{token.ScopeCheck}})
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+runnerproto.ResultsPath, func(rw http.ResponseWriter, r *http.Request) {
		c, err := v.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), time.Now())
		if err != nil {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		var res runnerproto.Result
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if dec.Decode(&res) != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		w.got <- posted{res, c}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return w, srv.URL
}

func (w *ward) next(t *testing.T) posted {
	t.Helper()
	select {
	case p := <-w.got:
		return p
	case <-time.After(maxWait): //lint:allow wallclock a runner that never posts is the failure under test
		t.Fatal("the runner posted no result")
	}
	return posted{}
}

func (w *ward) none(t *testing.T) {
	t.Helper()
	select {
	case p := <-w.got:
		t.Fatalf("the runner posted a result for a request that it must refuse: %+v", p.res)
	default:
	}
}

func request(id string, timeout int, argv ...string) []byte {
	c := runnerproto.Check{Name: "unit", Run: argv, TimeoutSeconds: timeout}
	body, _ := json.Marshal(runnerproto.Request{RequestID: id, Repo: repo, Tree: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40), Check: c, CheckHash: c.Hash(), Env: "env1"})
	return body
}

func send(t *testing.T, url string, secret, body []byte, sig string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(runnerproto.SignatureHeader, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// Run runs the suite against the runner that impl.New returns.
func Run(t *testing.T, impl Impl) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("suite-secret-0123456789")
	w, wardURL := newWard(t, pub)
	srv := httptest.NewServer(impl.New(t, Env{Secret: secret, WardURL: wardURL, Kid: kid, Key: priv}))
	t.Cleanup(srv.Close)

	t.Run("pass", func(t *testing.T) {
		body := request("r-pass", 60, "sh", "-c", "exit 0")
		if c := send(t, srv.URL, secret, body, runnerproto.Sign(secret, body)); c/100 != 2 {
			t.Fatalf("answered %d, want 2xx", c)
		}
		p := w.next(t)
		if p.res.RequestID != "r-pass" || p.res.Status != runnerproto.Pass || p.claims.Repo != repo || p.claims.Scope != token.ScopeCheck {
			t.Errorf("got %+v with claims %+v", p.res, p.claims)
		}
	})
	t.Run("fail", func(t *testing.T) {
		body := request("r-fail", 60, "sh", "-c", "echo broken; exit 1")
		if c := send(t, srv.URL, secret, body, runnerproto.Sign(secret, body)); c/100 != 2 {
			t.Fatalf("answered %d, want 2xx", c)
		}
		p := w.next(t)
		if p.res.RequestID != "r-fail" || p.res.Status != runnerproto.Fail || p.res.Summary == "" || len(p.res.Summary) > runnerproto.MaxSummary {
			t.Errorf("got %+v", p.res)
		}
	})
	t.Run("timeout fails the check", func(t *testing.T) {
		body := request("r-slow", 1, "sh", "-c", "sleep 300")
		if c := send(t, srv.URL, secret, body, runnerproto.Sign(secret, body)); c/100 != 2 {
			t.Fatalf("answered %d, want 2xx", c)
		}
		if p := w.next(t); p.res.RequestID != "r-slow" || p.res.Status != runnerproto.Fail {
			t.Errorf("got %+v", p.res)
		}
	})
	t.Run("refuses a request that is not authentic", func(t *testing.T) {
		body := request("r-bad", 60, "sh", "-c", "exit 0")
		if c := send(t, srv.URL, secret, body, runnerproto.Sign([]byte("another-secret-0123456"), body)); c != http.StatusUnauthorized {
			t.Errorf("bad signature: answered %d, want 401", c)
		}
		if c := send(t, srv.URL, secret, body, ""); c != http.StatusUnauthorized {
			t.Errorf("no signature: answered %d, want 401", c)
		}
		tampered := bytes.Replace(request("r-hash", 60, "sh", "-c", "exit 0"), []byte(`"check_hash":"`), []byte(`"check_hash":"00`), 1)
		if c := send(t, srv.URL, secret, tampered, runnerproto.Sign(secret, tampered)); c/100 != 4 {
			t.Errorf("wrong check_hash: answered %d, want 4xx", c)
		}
		w.none(t)
	})
}
