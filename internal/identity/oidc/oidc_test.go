package oidc_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/identity"
	"github.com/majorcontext/ward/internal/identity/identitytest"
	"github.com/majorcontext/ward/internal/identity/oidc"
)

var _ identity.Authenticator = (*oidc.Authenticator)(nil)

const (
	issuer = "https://idp.example.test"
	aud    = "aud-tag"
)

var now = time.Unix(1_000_000, 0)

func claims(email string, exp time.Time) map[string]any {
	return map[string]any{"iss": issuer, "aud": []string{aud}, "email": email, "exp": exp.Unix(), "nbf": now.Add(-time.Minute).Unix()}
}

func newAuth(t *testing.T, iss *identitytest.Issuer) *oidc.Authenticator {
	t.Helper()
	a, err := oidc.New(oidc.Config{Issuer: issuer, Audience: aud, KeysURL: iss.URL, Now: func() time.Time { return now },
		Rules: []identity.Rule{{Principals: []string{"@Dev.test"}, Repos: []string{identitytest.RepoA}}}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func request(jwt string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://example.test/", nil)
	r.Header.Set("Authorization", "Bearer "+jwt)
	return r
}

func TestConformance(t *testing.T) {
	t.Parallel()
	identitytest.Run(t, func(t *testing.T) identitytest.Fixture {
		iss, rogue := identitytest.NewIssuer(t, "k1"), identitytest.NewIssuer(t, "k1")
		return identitytest.Fixture{Auth: newAuth(t, iss),
			Valid:   func(string) *http.Request { return request(iss.Sign(t, claims("dev@dev.test", now.Add(time.Hour)))) },
			Expired: func(string) *http.Request { return request(iss.Sign(t, claims("dev@dev.test", now.Add(-time.Second)))) },
			Forged:  func(string) *http.Request { return request(rogue.Sign(t, claims("dev@dev.test", now.Add(time.Hour)))) }}
	})
}

func TestRefusesClaimsOfAnotherApplicationOrPrincipal(t *testing.T) {
	t.Parallel()
	key := identitytest.NewIssuer(t, "k1")
	a := newAuth(t, key)
	for name, mutate := range map[string]func(map[string]any){
		"valid":           func(map[string]any) {},
		"audience":        func(c map[string]any) { c["aud"] = []string{"other"} },
		"issuer":          func(c map[string]any) { c["iss"] = "https://other.example.test" },
		"not yet":         func(c map[string]any) { c["nbf"] = now.Add(time.Minute).Unix() },
		"no email":        func(c map[string]any) { delete(c, "email") },
		"not on the list": func(c map[string]any) { c["email"] = "dev@other.test" },
	} {
		c := claims("dev@dev.test", now.Add(time.Hour))
		mutate(c)
		_, err := a.Authenticate(request(key.Sign(t, c)), identitytest.RepoA)
		if (name == "valid") != (err == nil) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

type failableKeys struct{ down bool }

func (f *failableKeys) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.down {
		return nil, errors.New("key set unreachable")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestRefusesKeysAnHourPastTheirLastGoodFetch(t *testing.T) {
	t.Parallel()
	iss := identitytest.NewIssuer(t, "k1")
	keys := &failableKeys{}
	at := now
	a, err := oidc.New(oidc.Config{Issuer: issuer, Audience: aud, KeysURL: iss.URL, Client: &http.Client{Transport: keys}, Now: func() time.Time { return at },
		Rules: []identity.Rule{{Principals: []string{"dev@dev.test"}, Repos: []string{identitytest.RepoA}}}})
	if err != nil {
		t.Fatal(err)
	}
	r := request(iss.Sign(t, claims("dev@dev.test", now.Add(3*time.Hour))))
	check := func(wantOK bool, after time.Duration) {
		t.Helper()
		at = now.Add(after)
		if _, err := a.Authenticate(r, identitytest.RepoA); (err == nil) != wantOK {
			t.Errorf("after %s with the key set down: err = %v, want ok = %v", after, err, wantOK)
		}
	}
	check(true, 0)
	keys.down = true
	check(true, 59*time.Minute)
	check(false, 61*time.Minute)
}

func TestDiscoveryCustomHeaderAndClaim(t *testing.T) {
	t.Parallel()
	iss := identitytest.NewIssuer(t, "k1")
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": iss.URL})
	}))
	t.Cleanup(disc.Close)
	a, err := oidc.New(oidc.Config{Issuer: disc.URL, Audience: aud, Header: "X-Identity-Jwt", Claim: "sub", Now: func() time.Time { return now },
		Rules: []identity.Rule{{Principals: []string{"user-1"}, Repos: []string{identitytest.RepoA}}}})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest(http.MethodGet, "http://example.test/", nil)
	r.Header.Set("X-Identity-Jwt", iss.Sign(t, map[string]any{"iss": disc.URL, "aud": aud, "sub": "user-1", "exp": now.Add(time.Hour).Unix()}))
	if c, err := a.Authenticate(r, identitytest.RepoA); err != nil || c.Subject != "user-1" {
		t.Errorf("claims = %+v, err = %v, want subject user-1", c, err)
	}
	r.Header.Set("X-Identity-Jwt", iss.Sign(t, map[string]any{"iss": disc.URL, "aud": aud, "sub": "User-1", "exp": now.Add(time.Hour).Unix()}))
	if _, err := a.Authenticate(r, identitytest.RepoA); err == nil {
		t.Error("an opaque claim matched with another case")
	}
}

func TestAcceptsES256AndRefusesAnUnlistedAlgorithm(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	set := map[string]any{"keys": []map[string]string{{"kid": "e1", "kty": "EC", "crv": "P-256", "x": enc(key.X.Bytes()), "y": enc(key.Y.Bytes())}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(set) }))
	t.Cleanup(srv.Close)
	head, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "e1"})
	body, _ := json.Marshal(claims("dev@dev.test", now.Add(time.Hour)))
	signed := enc(head) + "." + enc(body)
	sum := sha256.Sum256([]byte(signed))
	r, s, _ := ecdsa.Sign(rand.Reader, key, sum[:])
	jwt := signed + "." + enc(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
	for algs, wantOK := range map[string]bool{"ES256": true, "RS256": false} {
		a, err := oidc.New(oidc.Config{Issuer: issuer, Audience: aud, KeysURL: srv.URL, Algs: []string{algs}, Now: func() time.Time { return now },
			Rules: []identity.Rule{{Principals: []string{"@dev.test"}, Repos: []string{identitytest.RepoA}}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Authenticate(request(jwt), identitytest.RepoA); (err == nil) != wantOK {
			t.Errorf("algorithms %s: err = %v, want ok = %v", algs, err, wantOK)
		}
	}
}
