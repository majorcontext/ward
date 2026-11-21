// Package token signs and verifies the caller tokens of ward.
package token

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

// ErrToken is the one error of a rejected token. It never says why.
var ErrToken = errors.New("token: not accepted")

type Scope string

const (
	ScopeRead     Scope = "read"
	ScopeOperator Scope = "operator"
	ScopeLand     Scope = "land"
	ScopeCheck    Scope = "check"
	ScopeRunner   Scope = "runner"
)

const prefix = "wd1."

// Claims name what a bearer may do. Subject is for logs and traces and grants nothing.
type Claims struct {
	Kid     string `json:"kid"`
	Scope   Scope  `json:"scope"`
	Repo    string `json:"repo,omitempty"`
	Subject string `json:"subject,omitempty"`
	Exp     int64  `json:"exp"`
}

func (c Claims) valid() bool {
	switch c.Scope {
	case ScopeRead, ScopeLand, ScopeCheck:
		return c.Repo != ""
	case ScopeOperator, ScopeRunner:
		return true
	}
	return false
}

// Signer mints tokens with an Ed25519 private key.
type Signer struct {
	Kid string
	Key ed25519.PrivateKey
}

// Mint signs c under the kid of the signer.
func (s Signer) Mint(c Claims) (string, error) {
	c.Kid = s.Kid
	if !c.valid() {
		return "", errors.New("token: claims are incomplete for the scope")
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	sig := ed25519.Sign(s.Key, []byte(payload))
	return prefix + payload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Key is a public key and the scopes that it may sign.
type Key struct {
	Kid    string
	Public ed25519.PublicKey
	Scopes []Scope
}

// Verifier accepts tokens that one of its keys signed.
type Verifier struct {
	keys map[string]Key
}

func NewVerifier(keys ...Key) *Verifier {
	v := &Verifier{keys: make(map[string]Key, len(keys))}
	for _, k := range keys {
		v.keys[k.Kid] = k
	}
	return v
}

// Verify returns the claims of tok when a known key signed it, the key may sign the scope,
// the claims are complete and now is before Exp.
func (v *Verifier) Verify(tok string, now time.Time) (Claims, error) {
	rest, ok := strings.CutPrefix(tok, prefix)
	payload, sigText, found := strings.Cut(rest, ".")
	if !ok || !found {
		return Claims{}, ErrToken
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Claims{}, ErrToken
	}
	var c Claims
	if json.Unmarshal(body, &c) != nil {
		return Claims{}, ErrToken
	}
	key, ok := v.keys[c.Kid]
	sig, err := base64.RawURLEncoding.DecodeString(sigText)
	if !ok || err != nil || len(key.Public) != ed25519.PublicKeySize || !ed25519.Verify(key.Public, []byte(payload), sig) {
		return Claims{}, ErrToken
	}
	if !slices.Contains(key.Scopes, c.Scope) || now.Unix() >= c.Exp || !c.valid() {
		return Claims{}, ErrToken
	}
	return c, nil
}
