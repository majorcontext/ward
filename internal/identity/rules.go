package identity

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/majorcontext/ward/internal/token"
)

// Rule gives the scope land on Repos to Principals. A principal is a subject text, or @domain for every email of the domain.
type Rule struct {
	Principals []string `json:"principals"`
	Repos      []string `json:"repos"`
}

// Validate checks the rules.
func Validate(rules []Rule) error {
	for i, r := range rules {
		if len(r.Principals) == 0 || len(r.Repos) == 0 {
			return fmt.Errorf("rule %d needs principals and repos", i)
		}
	}
	return nil
}

// Allows reports whether the rules let subject land on repo. An email is not case sensitive, so with email true
// the match ignores case. Any other claim is an opaque identifier and matches exactly.
func Allows(rules []Rule, subject, repo string, email bool) bool {
	fold := func(v string) string {
		if email {
			return strings.ToLower(v)
		}
		return v
	}
	subject = fold(subject)
	domain := ""
	if i := strings.LastIndex(subject, "@"); email && i >= 0 {
		domain = subject[i:]
	}
	return slices.ContainsFunc(rules, func(r Rule) bool {
		return slices.Contains(r.Repos, repo) &&
			slices.ContainsFunc(r.Principals, func(p string) bool { p = fold(p); return p == subject || domain != "" && p == domain })
	})
}

// Claims is the claims of a human identity that may land on repo.
func Claims(repo, subject string, exp int64) token.Claims {
	return token.Claims{Scope: token.ScopeLand, Repo: repo, Subject: subject, Exp: exp}
}

type chain []Authenticator

// Chain returns an Authenticator that answers with the first claims that one of auths gives, and token.ErrToken when
// none gives any. It skips a nil member.
func Chain(auths ...Authenticator) Authenticator {
	var live chain
	for _, a := range auths {
		if a != nil {
			live = append(live, a)
		}
	}
	return live
}

func (c chain) Authenticate(r *http.Request, repo string) (token.Claims, error) {
	for _, a := range c {
		if cl, err := a.Authenticate(r, repo); err == nil {
			return cl, nil
		}
	}
	return token.Claims{}, token.ErrToken
}
