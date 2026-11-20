package bearer_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/identity"
	"github.com/majorcontext/ward/internal/identity/bearer"
	"github.com/majorcontext/ward/internal/identity/identitytest"
	"github.com/majorcontext/ward/internal/token"
)

var _ identity.Authenticator = bearer.Authenticator{}

func TestConformance(t *testing.T) {
	t.Parallel()
	identitytest.Run(t, func(t *testing.T) identitytest.Fixture {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		_, rogue, _ := ed25519.GenerateKey(rand.Reader)
		now := time.Unix(1_000_000, 0)
		auth := bearer.Authenticator{Verifier: token.NewVerifier(token.Key{Kid: "k", Public: pub, Scopes: []token.Scope{token.ScopeLand}}), Now: func() time.Time { return now }}
		req := func(key ed25519.PrivateKey, repo string, exp time.Time) *http.Request {
			tok, err := token.Signer{Kid: "k", Key: key}.Mint(token.Claims{Scope: token.ScopeLand, Repo: repo, Exp: exp.Unix()})
			if err != nil {
				t.Fatal(err)
			}
			r, _ := http.NewRequest(http.MethodGet, "http://example.test/", nil)
			r.Header.Set("Authorization", "Bearer "+tok)
			return r
		}
		return identitytest.Fixture{Auth: auth,
			Valid:   func(repo string) *http.Request { return req(priv, repo, now.Add(time.Hour)) },
			Expired: func(repo string) *http.Request { return req(priv, repo, now.Add(-time.Second)) },
			Forged:  func(repo string) *http.Request { return req(rogue, repo, now.Add(time.Hour)) }}
	})
}
