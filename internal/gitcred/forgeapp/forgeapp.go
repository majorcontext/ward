// Package forgeapp is the gitcred.Credential of a GitHub App installation. It signs a JWT with the key of the app, exchanges it
// for an installation token, and keeps the token until shortly before it expires.
package forgeapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/ward/internal/gitcred"
	"github.com/majorcontext/ward/internal/gitremote"
)

const (
	// DefaultAPI is the API base URL when the configuration names none.
	DefaultAPI    = "https://api.github.com"
	Username      = "x-access-token"
	jwtBackdate   = 60 * time.Second
	jwtLife       = 9 * time.Minute
	refreshMargin = 5 * time.Minute
	maxTokenBody  = 1 << 20
	callBudget    = 30 * time.Second
)

// Clock tells the time, so a test can move it.
type Clock interface{ Now() time.Time }

// App is a GitHub App installation that supplies the credential.
type App struct {
	AppID          int64
	InstallationID int64
	KeyFile        string
}

var _ gitcred.Credential = (*Source)(nil)

func ReadKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("unreadable")
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
	}
	return nil, errors.New("not an RSA private key")
}

// Source exchanges an app JWT for an installation token and caches the token until shortly before it expires.
type Source struct {
	app   App
	api   string
	dir   string
	clock Clock
	http  *http.Client

	mu      sync.Mutex
	cached  string
	expires time.Time
}

func New(app App, api, dir string, clock Clock) *Source {
	if api == "" {
		api = DefaultAPI
	}
	return &Source{app: app, api: strings.TrimRight(api, "/"), dir: dir, clock: clock, http: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (a *Source) signJWT(now time.Time) (string, error) {
	key, err := ReadKey(a.app.KeyFile)
	if err != nil {
		return "", &gitcred.Error{Kind: gitcred.Local, Msg: "the app key file is not usable"}
	}
	payload, _ := json.Marshal(map[string]any{"iat": now.Add(-jwtBackdate).Unix(), "exp": now.Add(jwtLife).Unix(), "iss": fmt.Sprint(a.app.AppID)})
	signing := b64([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", &gitcred.Error{Kind: gitcred.Local, Msg: "the app JWT could not be signed"}
	}
	return signing + "." + b64(sig), nil
}

// token returns an installation token that stays valid for at least the refresh margin.
func (a *Source) token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock.Now()
	if a.cached != "" && now.Before(a.expires.Add(-refreshMargin)) {
		return a.cached, nil
	}
	jwt, err := a.signJWT(now)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", a.api, a.app.InstallationID), nil)
	if err != nil {
		return "", &gitcred.Error{Kind: gitcred.Local, Msg: "the token URL is not valid"}
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", &gitcred.Error{Kind: gitcred.Unreachable, Msg: "the token endpoint did not answer"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", &gitcred.Error{Kind: gitcred.Auth, Msg: fmt.Sprintf("the token endpoint refused the app JWT (status %d)", resp.StatusCode)}
	case resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK:
		return "", &gitcred.Error{Kind: gitcred.Unreachable, Msg: fmt.Sprintf("the token endpoint answered status %d", resp.StatusCode)}
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if json.Unmarshal(body, &out) != nil || out.Token == "" || out.ExpiresAt.IsZero() {
		return "", &gitcred.Error{Kind: gitcred.Unreachable, Msg: "the token endpoint answered a body that is not a token"}
	}
	a.cached, a.expires = out.Token, out.ExpiresAt
	return a.cached, nil
}

// Use writes a token to a file in the directory of the source and returns it as the password of the user x-access-token.
func (a *Source) Use(ctx context.Context) (gitremote.Cred, func(), error) {
	tok, err := a.token(ctx)
	if err != nil {
		return gitremote.Cred{}, nil, err
	}
	f, err := os.CreateTemp(a.dir, "askpass-*")
	if err != nil {
		return gitremote.Cred{}, nil, &gitcred.Error{Kind: gitcred.Local, Msg: "the token file could not be created"}
	}
	_, werr := f.WriteString(tok)
	if cerr := f.Close(); werr != nil || cerr != nil {
		_ = os.Remove(f.Name())
		return gitremote.Cred{}, nil, &gitcred.Error{Kind: gitcred.Local, Msg: "the token file could not be written"}
	}
	return gitremote.Cred{AskUser: Username, AskFile: f.Name()}, func() { _ = os.Remove(f.Name()) }, nil
}
