package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	appID          = 7
	installationID = 42
)

// fakeGitHub answers the installation token route and makes the remote accept the token that it issued last.
type fakeGitHub struct {
	srv     *httptest.Server
	keyFile string
	mu      sync.Mutex
	calls   int
	status  int
}

func newFakeGitHub(t *testing.T, r *remote, clk *fakeClock) *fakeGitHub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	g := &fakeGitHub{keyFile: filepath.Join(t.TempDir(), "app.pem")}
	if err := os.WriteFile(g.keyFile, pemKey, 0o600); err != nil {
		t.Fatal(err)
	}
	r.user = "x-access-token"
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != fmt.Sprintf("/app/installations/%d/access_tokens", installationID) {
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := checkAppJWT(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), &key.PublicKey, clk.Now()); err != nil {
			t.Errorf("app JWT: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls++
		if g.status != 0 {
			w.WriteHeader(g.status)
			return
		}
		tok := fmt.Sprintf("ghs_issued%d", g.calls)
		r.pass.set(tok)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": tok, "expires_at": clk.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) issued() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *fakeGitHub) refuse(status int) {
	g.mu.Lock()
	g.status = status
	g.mu.Unlock()
}

func checkAppJWT(jwt string, pub *rsa.PublicKey, now time.Time) error {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return fmt.Errorf("not three parts")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	head, _ := base64.RawURLEncoding.DecodeString(parts[0])
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var h struct{ Alg string }
	var c struct {
		Iat, Exp int64
		Iss      string
	}
	if json.Unmarshal(head, &h) != nil || json.Unmarshal(payload, &c) != nil {
		return fmt.Errorf("not JSON")
	}
	switch {
	case h.Alg != "RS256":
		return fmt.Errorf("alg %q", h.Alg)
	case c.Iss != fmt.Sprint(appID):
		return fmt.Errorf("iss %q", c.Iss)
	case c.Iat != now.Unix()-60:
		return fmt.Errorf("iat %d is not 60 s before now", c.Iat)
	case c.Exp <= now.Unix() || c.Exp > now.Unix()+600:
		return fmt.Errorf("exp %d is not within 10 minutes", c.Exp)
	}
	return nil
}

func TestMirrorPushesWithAnInstallationTokenAndRefreshesItWhenItExpires(t *testing.T) {
	h := newHarness(t)
	clk := newFakeClock(func() { h.note("timer", "") })
	gh := newFakeGitHub(t, h.remote, clk)
	p := h.startWard(wardOpts{clock: clk, app: gh})
	h.skip()

	c1 := h.commit("a.txt", "1")
	h.push("feature/a", c1, false)
	h.relay(p)
	h.waitTip("feature/a", c1)
	if n := gh.issued(); n != 1 {
		t.Fatalf("tokens issued = %d for the reconcile and a push inside the token life, want 1", n)
	}

	clk.advance(56 * time.Minute)
	c2 := h.commit("a.txt", "2")
	h.push("feature/a", c2, false)
	h.relay(p)
	h.waitTip("feature/a", c2)
	if n := gh.issued(); n != 2 {
		t.Fatalf("tokens issued = %d after the first token neared its expiry, want 2", n)
	}
}

func TestMirrorShowsRefusedTokenExchangeAsRemoteAuthAndRecovers(t *testing.T) {
	h := newHarness(t)
	clk := newFakeClock(func() { h.note("timer", "") })
	gh := newFakeGitHub(t, h.remote, clk)
	p := h.startWard(wardOpts{clock: clk, app: gh})
	h.skip()

	gh.refuse(http.StatusUnauthorized)
	clk.advance(56 * time.Minute)
	c1 := h.commit("a.txt", "1")
	h.push("feature/a", c1, false)
	h.relay(p)
	h.waitRef(p, "feature/a", func(r refView) bool {
		return r.State == "failing" && r.Failure == "remote_auth" && !strings.Contains(r.Message, "ghs_")
	})

	gh.refuse(0)
	h.waitFor("a retry timer", func() bool { return clk.pending() >= 1 })
	clk.advance(10 * time.Minute)
	h.waitTip("feature/a", c1)
}
