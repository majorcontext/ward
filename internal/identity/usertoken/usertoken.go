// Package usertoken authenticates a request by a per-user token that an operator issued. ward keeps only the hash of
// the token, so the identity needs no external provider.
package usertoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/identity"
	"github.com/majorcontext/ward/internal/token"
)

// Prefix starts every token, so that a credential of another kind never reaches the store.
const Prefix = "wdu_"

// ErrNotFound is the answer of a store for an unknown hash or ID. ErrExists is the answer for a repeated ID or hash.
var (
	ErrNotFound = errors.New("usertoken: not found")
	ErrExists   = errors.New("usertoken: exists")
)

// ScopeLand is the only scope of a per-user token.
const ScopeLand = "land"

// Record is a per-user token. The store keeps its hash and never the token. ExpiresAt and RevokedAt are zero when the
// token has no expiry or is not revoked.
type Record struct {
	ID, Principal, Scope string
	Repos                []string
	CreatedAt, ExpiresAt time.Time
	RevokedAt            time.Time
}

// Store keeps per-user tokens. hash is the lowercase hex SHA-256 of the token text.
type Store interface {
	// CreateUserToken stores r under hash. ErrExists when r.ID or hash exists.
	CreateUserToken(ctx context.Context, hash string, r Record) error
	// UserTokenByHash returns the record under hash, revoked or not, or ErrNotFound.
	UserTokenByHash(ctx context.Context, hash string) (Record, error)
	// UserTokens lists every record, newest first, ties by ID descending.
	UserTokens(ctx context.Context) ([]Record, error)
	// RevokeUserToken sets RevokedAt of id to at once. A repeat keeps the first time and is not an error.
	// ErrNotFound for an unknown id.
	RevokeUserToken(ctx context.Context, id string, at time.Time) error
}

// Valid reports whether a record may exist: the scope land on at least one repository.
func Valid(scope string, repos []string) bool { return scope == ScopeLand && len(repos) > 0 }

// New returns a token text, the ID of its record and the hash that the store keeps.
func New() (tok, id, hash string, err error) {
	raw := make([]byte, 40)
	if _, err = rand.Read(raw); err != nil {
		return "", "", "", err
	}
	tok = Prefix + base64.RawURLEncoding.EncodeToString(raw[8:])
	return tok, "ut_" + hex.EncodeToString(raw[:8]), Hash(tok), nil
}

// Hash is the lowercase hex SHA-256 of tok.
func Hash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Credential is the token of the header Authorization: a Bearer token, or the password of a Basic credential.
func Credential(r *http.Request) string {
	if scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(tok)
	}
	if _, pass, ok := r.BasicAuth(); ok {
		return pass
	}
	return ""
}

// Authenticator finds the record of the credential by hash, and refuses a token that is unknown, revoked or expired.
type Authenticator struct {
	Store Store
	Now   func() time.Time
}

var _ identity.Authenticator = Authenticator{}

func (a Authenticator) Authenticate(r *http.Request, repo string) (token.Claims, error) {
	tok := Credential(r)
	if !strings.HasPrefix(tok, Prefix) {
		return token.Claims{}, token.ErrToken
	}
	rec, err := a.Store.UserTokenByHash(r.Context(), Hash(tok))
	now := a.Now()
	if err != nil || !rec.RevokedAt.IsZero() || !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt) ||
		!Valid(rec.Scope, rec.Repos) || !slices.Contains(rec.Repos, repo) {
		return token.Claims{}, token.ErrToken
	}
	exp := now.Add(time.Hour)
	if !rec.ExpiresAt.IsZero() {
		exp = rec.ExpiresAt
	}
	return identity.Claims(repo, rec.Principal, exp.Unix()), nil
}
