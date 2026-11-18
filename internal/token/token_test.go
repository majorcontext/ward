package token_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/token"
)

func TestVerify(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1000, 0)
	v := token.NewVerifier(token.Key{Kid: "k", Public: pub, Scopes: []token.Scope{token.ScopeRead}})
	mint := func(s token.Signer, c token.Claims) string {
		tok, err := s.Mint(c)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	good := token.Signer{Kid: "k", Key: key}
	valid := mint(good, token.Claims{Scope: token.ScopeRead, Repo: "native:r", Exp: 2000})
	tests := []struct {
		name string
		tok  string
		ok   bool
	}{
		{"valid", valid, true},
		{"expired at exp", mint(good, token.Claims{Scope: token.ScopeRead, Repo: "r", Exp: 1000}), false},
		{"scope the key may not sign", mint(good, token.Claims{Scope: token.ScopeOperator, Exp: 2000}), false},
		{"other key", mint(token.Signer{Kid: "k", Key: otherKey}, token.Claims{Scope: token.ScopeRead, Repo: "r", Exp: 2000}), false},
		{"unknown kid", mint(token.Signer{Kid: "x", Key: key}, token.Claims{Scope: token.ScopeRead, Repo: "r", Exp: 2000}), false},
		{"tampered payload", "wd1.e30." + strings.SplitN(valid, ".", 3)[2], false},
		{"no prefix", strings.TrimPrefix(valid, "wd1."), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.Verify(tt.tok, now)
			if (err == nil) != tt.ok {
				t.Fatalf("Verify err = %v, want ok = %v", err, tt.ok)
			}
		})
	}
	for _, scope := range []token.Scope{token.ScopeRead, token.ScopeLand, token.ScopeCheck} {
		if _, err := good.Mint(token.Claims{Scope: scope, Exp: 2000}); err == nil {
			t.Fatalf("a %s token with no repo was minted", scope)
		}
	}
}
