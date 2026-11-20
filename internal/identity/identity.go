// Package identity maps the credential of a request to the claims of a caller token.
package identity

import (
	"net/http"

	"github.com/majorcontext/ward/internal/token"
)

// Authenticator reads the credential of r and returns the claims that it gives. repo is the repository text that
// the call names, empty when the call names none. A credential that is missing, forged, expired or not allowed for
// repo answers token.ErrToken and nothing else. The claims never hold a scope above what the credential gives.
type Authenticator interface {
	Authenticate(r *http.Request, repo string) (token.Claims, error)
}
