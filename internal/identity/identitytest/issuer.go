package identitytest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issuer signs JWTs with a new RSA key and serves the key set of it.
type Issuer struct {
	Kid string
	Key *rsa.PrivateKey
	// URL is the address of the key set.
	URL string
}

// NewIssuer starts a key set server that t stops.
func NewIssuer(t testing.TB, kid string) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	set := map[string]any{"keys": []map[string]string{{"kid": kid, "kty": "RSA", "alg": "RS256", "use": "sig",
		"n": enc(key.N.Bytes()), "e": enc([]byte{1, 0, 1})}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(set) }))
	t.Cleanup(srv.Close)
	return &Issuer{Kid: kid, Key: key, URL: srv.URL}
}

// Sign returns an RS256 JWT with the claims, under the kid of the issuer.
func (i *Issuer) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": i.Kid, "typ": "JWT"})
	body, _ := json.Marshal(claims)
	signed := enc(head) + "." + enc(body)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.Key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + enc(sig)
}
