// Package bearer authenticates a request by a signed caller token in the header Authorization.
package bearer

import (
	"net/http"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/token"
)

// Authenticator verifies the bearer token of the request.
type Authenticator struct {
	Verifier *token.Verifier
	Now      func() time.Time
}

func (a Authenticator) Authenticate(r *http.Request, _ string) (token.Claims, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || a.Verifier == nil {
		return token.Claims{}, token.ErrToken
	}
	return a.Verifier.Verify(tok, a.Now())
}
