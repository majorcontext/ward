// Package identitytest holds the conformance suite of identity.Authenticator and a signer of JWTs.
package identitytest

import (
	"errors"
	"net/http"
	"testing"

	"github.com/majorcontext/ward/internal/identity"
	"github.com/majorcontext/ward/internal/token"
)

// Fixture is an Authenticator and the requests that it must judge. Each request func returns a new request.
type Fixture struct {
	Auth identity.Authenticator
	// Valid carries a credential that gives land on repo.
	Valid func(repo string) *http.Request
	// Expired carries a credential that gave land on repo and has expired.
	Expired func(repo string) *http.Request
	// Forged carries a credential that gives land on repo and is signed by a key that the Authenticator does not trust.
	Forged func(repo string) *http.Request
	// Revoked carries a credential that gave land on repo and was revoked. It is nil for a credential that cannot be revoked.
	Revoked func(repo string) *http.Request
}

// RepoA is the repository that the credentials of a fixture cover. RepoB is one that they do not cover.
const (
	RepoA = "native:repo_A"
	RepoB = "native:repo_B"
)

// Run runs the suite.
func Run(t *testing.T, newFixture func(t *testing.T) Fixture) {
	t.Helper()
	refuse := func(t *testing.T, f Fixture, r *http.Request) {
		t.Helper()
		if _, err := f.Auth.Authenticate(r, RepoA); !errors.Is(err, token.ErrToken) {
			t.Fatalf("error = %v, want token.ErrToken", err)
		}
	}
	t.Run("accepts a credential with its scope and repository", func(t *testing.T) {
		f := newFixture(t)
		c, err := f.Auth.Authenticate(f.Valid(RepoA), RepoA)
		if err != nil || c.Scope != token.ScopeLand || c.Repo != RepoA {
			t.Errorf("claims = %+v, err = %v, want land on %s", c, err, RepoA)
		}
	})
	t.Run("refuses a request with no credential", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "http://example.test/", nil)
		refuse(t, newFixture(t), r)
	})
	t.Run("refuses an expired credential", func(t *testing.T) {
		f := newFixture(t)
		refuse(t, f, f.Expired(RepoA))
	})
	t.Run("refuses a revoked credential", func(t *testing.T) {
		f := newFixture(t)
		if f.Revoked == nil {
			t.Skip("the credential cannot be revoked")
		}
		refuse(t, f, f.Revoked(RepoA))
	})
	t.Run("refuses a forged credential", func(t *testing.T) {
		f := newFixture(t)
		refuse(t, f, f.Forged(RepoA))
	})
	t.Run("never gives the claims of another repository", func(t *testing.T) {
		f := newFixture(t)
		if c, err := f.Auth.Authenticate(f.Valid(RepoA), RepoB); err == nil && c.Repo == RepoB {
			t.Errorf("claims = %+v for %s, want a refusal or the repository of the credential", c, RepoB)
		}
	})
}
