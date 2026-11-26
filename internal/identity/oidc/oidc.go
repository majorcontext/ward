// Package oidc authenticates a request by a signed OIDC JWT from any issuer, and maps a claim of the JWT to the scope
// land on a repository with rules. An identity-aware proxy that signs a JWT with a key set is one configuration of it.
package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/ward/internal/identity"
	"github.com/majorcontext/ward/internal/token"
)

const (
	// refreshAfter is the age of the key set at which a request starts a fetch. The cached keys stay in use until
	// staleAfter, so one failed fetch does not refuse a sign-in.
	refreshAfter = 45 * time.Minute
	staleAfter   = 75 * time.Minute
	// unknownKidWait limits the fetches that a token with an unknown kid can cause.
	unknownKidWait = time.Minute
	keysMax        = 64 << 10
)

// Config is the deployment of one issuer.
type Config struct {
	// Issuer is the exact value of the iss claim.
	Issuer string
	// Audience must be one of the aud claims.
	Audience string
	// KeysURL is the address of the key set. When it is empty, the key set is the jwks_uri of the discovery
	// document at <Issuer>/.well-known/openid-configuration.
	KeysURL string
	// Header names the header that carries the JWT. Default Authorization, as a Bearer token or the password of a
	// Basic credential. Any other header carries the bare JWT.
	Header string
	// Claim names the claim that holds the subject text that the rules match. Default email.
	Claim string
	// Algs lists the accepted signature algorithms: RS256, RS384, RS512, ES256 or ES384. Default RS256.
	Algs   []string
	Rules  []identity.Rule
	Client *http.Client
	Now    func() time.Time
}

// Authenticator verifies the JWT and applies the rules.
type Authenticator struct {
	cfg Config

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time
	good    time.Time
	// fetching is open while a fetch runs, so that one fetch runs at a time and none holds mu.
	fetching chan struct{}
}

var hashes = map[string]crypto.Hash{"RS256": crypto.SHA256, "RS384": crypto.SHA384, "RS512": crypto.SHA512, "ES256": crypto.SHA256, "ES384": crypto.SHA384}

// New checks cfg. A human identity may only land: it reads no status, posts no check result and mints no token.
func New(cfg Config) (*Authenticator, error) {
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("oidc: the issuer and the audience are required")
	}
	if err := identity.Validate(cfg.Rules); err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}
	if len(cfg.Algs) == 0 {
		cfg.Algs = []string{"RS256"}
	}
	for _, a := range cfg.Algs {
		if _, ok := hashes[a]; !ok {
			return nil, fmt.Errorf("oidc: algorithm %q is not supported", a)
		}
	}
	if cfg.Header == "" {
		cfg.Header = "Authorization"
	}
	if cfg.Claim == "" {
		cfg.Claim = "email"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Authenticator{cfg: cfg}, nil
}

type jwtClaims struct {
	Iss     string
	Aud     []string
	Exp     int64
	Nbf     int64
	Subject string
}

func (a *Authenticator) token(r *http.Request) string {
	v := r.Header.Get(a.cfg.Header)
	if !strings.EqualFold(a.cfg.Header, "Authorization") {
		return v
	}
	if scheme, tok, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(tok)
	}
	if _, pass, ok := r.BasicAuth(); ok {
		return pass
	}
	return ""
}

func (a *Authenticator) Authenticate(r *http.Request, repo string) (token.Claims, error) {
	jc, err := a.verify(a.token(r))
	if err != nil {
		return token.Claims{}, token.ErrToken
	}
	if !identity.Allows(a.cfg.Rules, jc.Subject, repo, a.cfg.Claim == "email") {
		return token.Claims{}, token.ErrToken
	}
	return identity.Claims(repo, jc.Subject, jc.Exp), nil
}

func (a *Authenticator) verify(token string) (jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, errors.New("not a JWT")
	}
	var head struct{ Alg, Kid string }
	var raw map[string]json.RawMessage
	if decode(parts[0], &head) != nil || decode(parts[1], &raw) != nil || !slices.Contains(a.cfg.Algs, head.Alg) {
		return jwtClaims{}, errors.New("bad JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	key, err2 := a.key(head.Kid)
	if err != nil || err2 != nil {
		return jwtClaims{}, errors.New("bad signature or key")
	}
	h := hashes[head.Alg]
	hash := h.New()
	hash.Write([]byte(parts[0] + "." + parts[1]))
	if !verifySig(head.Alg, key, h, hash.Sum(nil), sig) {
		return jwtClaims{}, errors.New("signature mismatch")
	}
	jc := jwtClaims{}
	_ = json.Unmarshal(raw["iss"], &jc.Iss)
	_ = json.Unmarshal(raw["exp"], &jc.Exp)
	_ = json.Unmarshal(raw["nbf"], &jc.Nbf)
	_ = json.Unmarshal(raw[a.cfg.Claim], &jc.Subject)
	var verified bool
	_ = json.Unmarshal(raw["email_verified"], &verified)
	var one string
	if json.Unmarshal(raw["aud"], &one) == nil {
		jc.Aud = []string{one}
	} else {
		_ = json.Unmarshal(raw["aud"], &jc.Aud)
	}
	now := a.cfg.Now().Unix()
	if jc.Iss != a.cfg.Issuer || !slices.Contains(jc.Aud, a.cfg.Audience) || jc.Exp == 0 || now >= jc.Exp || now < jc.Nbf || jc.Subject == "" || (a.cfg.Claim == "email" && !verified) {
		return jwtClaims{}, errors.New("claims refused")
	}
	return jc, nil
}

func verifySig(alg string, key crypto.PublicKey, h crypto.Hash, sum, sig []byte) bool {
	switch k := key.(type) {
	case *rsa.PublicKey:
		return strings.HasPrefix(alg, "RS") && rsa.VerifyPKCS1v15(k, h, sum, sig) == nil
	case *ecdsa.PublicKey:
		size := (k.Curve.Params().BitSize + 7) / 8
		return strings.HasPrefix(alg, "ES") && len(sig) == 2*size &&
			ecdsa.Verify(k, sum, new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:]))
	}
	return false
}

func decode(part string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// key returns the public key of kid. A request starts a fetch of the key set when the cache is older than refreshAfter or kid
// is unknown, at most once a minute and one at a time, outside the lock. A request that holds kid in the cache does not wait for it. It refuses a cache that staleAfter has
// passed since the last good fetch.
func (a *Authenticator) key(kid string) (crypto.PublicKey, error) {
	a.mu.Lock()
	now := a.cfg.Now()
	_, known := a.keys[kid]
	var wait chan struct{}
	if (now.Sub(a.good) >= refreshAfter || !known) && (a.fetched.IsZero() || now.Sub(a.fetched) >= unknownKidWait) && a.fetching == nil {
		wait = make(chan struct{})
		a.fetching, a.fetched = wait, now
	}
	running := a.fetching
	a.mu.Unlock()
	switch {
	case wait != nil && known:
		go a.refresh(wait, now)
	case wait != nil:
		a.refresh(wait, now)
	case !known && running != nil:
		<-running
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	k, known := a.keys[kid]
	if !known || now.Sub(a.good) >= staleAfter {
		return nil, errors.New("unknown or stale key")
	}
	return k, nil
}

// refresh fetches the key set and then ends the fetch that wait stands for, also when the fetch panics.
func (a *Authenticator) refresh(wait chan struct{}, now time.Time) {
	var keys map[string]crypto.PublicKey
	defer func() {
		a.mu.Lock()
		if keys != nil {
			a.keys, a.good = keys, now
		}
		a.fetching = nil
		a.mu.Unlock()
		close(wait)
	}()
	keys, _ = a.fetch()
}

func (a *Authenticator) getJSON(url string, v any) error {
	resp, err := a.cfg.Client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, keysMax)).Decode(v)
}

func (a *Authenticator) fetch() (map[string]crypto.PublicKey, error) {
	url := a.cfg.KeysURL
	if url == "" {
		var doc struct {
			JWKSURI string `json:"jwks_uri"`
		}
		if err := a.getJSON(strings.TrimSuffix(a.cfg.Issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil || doc.JWKSURI == "" {
			return nil, fmt.Errorf("discovery: %v", err)
		}
		url = doc.JWKSURI
	}
	var set struct {
		Keys []struct{ Kid, Kty, N, E, Crv, X, Y string } `json:"keys"`
	}
	if err := a.getJSON(url, &set); err != nil {
		return nil, fmt.Errorf("key set: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Kid == "" {
			continue
		}
		if pub := parseKey(k.Kty, k.N, k.E, k.Crv, k.X, k.Y); pub != nil {
			keys[k.Kid] = pub
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("key set: no usable key")
	}
	return keys, nil
}

func parseKey(kty, n, e, crv, x, y string) crypto.PublicKey {
	b := func(s string) *big.Int {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return nil
		}
		return new(big.Int).SetBytes(raw)
	}
	switch kty {
	case "RSA":
		N, E := b(n), b(e)
		if N == nil || E == nil || !E.IsInt64() || E.Int64() > 1<<31-1 {
			return nil
		}
		return &rsa.PublicKey{N: N, E: int(E.Int64())}
	case "EC":
		curve := map[string]elliptic.Curve{"P-256": elliptic.P256(), "P-384": elliptic.P384()}[crv]
		X, Y := b(x), b(y)
		if curve == nil || X == nil || Y == nil || !curve.IsOnCurve(X, Y) {
			return nil
		}
		return &ecdsa.PublicKey{Curve: curve, X: X, Y: Y}
	}
	return nil
}
