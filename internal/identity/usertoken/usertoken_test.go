package usertoken_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/identity/identitytest"
	"github.com/majorcontext/ward/internal/identity/usertoken"
	"github.com/majorcontext/ward/internal/usertokentest"
)

func TestConformance(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	identitytest.Run(t, func(t *testing.T) identitytest.Fixture {
		store := usertokentest.NewMem()
		issue := func(repo string, exp time.Time, revoked bool) *http.Request {
			tok, id, hash, err := usertoken.New()
			if err != nil {
				t.Fatal(err)
			}
			rec := usertoken.Record{ID: id, Principal: "dev@example.test", Scope: usertoken.ScopeLand, Repos: []string{repo}, ExpiresAt: exp}
			if err := store.CreateUserToken(t.Context(), hash, rec); err != nil {
				t.Fatal(err)
			}
			if revoked {
				_ = store.RevokeUserToken(t.Context(), id, now)
			}
			r, _ := http.NewRequest(http.MethodGet, "http://example.test/", nil)
			r.SetBasicAuth("x", tok)
			return r
		}
		forged, _ := http.NewRequest(http.MethodGet, "http://example.test/", nil)
		forged.Header.Set("Authorization", "Bearer "+usertoken.Prefix+"forged")
		return identitytest.Fixture{Auth: usertoken.Authenticator{Store: store, Now: func() time.Time { return now }},
			Valid:   func(repo string) *http.Request { return issue(repo, now.Add(time.Hour), false) },
			Expired: func(repo string) *http.Request { return issue(repo, now.Add(-time.Second), false) },
			Revoked: func(repo string) *http.Request { return issue(repo, time.Time{}, true) },
			Forged:  func(string) *http.Request { return forged }}
	})
}

func TestMemStoreConformance(t *testing.T) {
	t.Parallel()
	usertokentest.Run(t, func(*testing.T) usertoken.Store { return usertokentest.NewMem() })
}
