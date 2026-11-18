package mirror

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
)

const (
	// DefaultGitHubAPI is the API base URL when the configuration names none.
	DefaultGitHubAPI = "https://api.github.com"
	appUsername      = "x-access-token"
	jwtBackdate      = 60 * time.Second
	jwtLife          = 9 * time.Minute
	refreshMargin    = 5 * time.Minute
	maxTokenBody     = 1 << 20
)

// GitHubApp is a GitHub App installation that supplies the remote credential.
type GitHubApp struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	KeyFile        string `json:"key_file"`
}

// credError is a failure to get the remote credential. Its text never holds a token or a key.
type credError struct {
	class Class
	msg   string
}

func (e *credError) Error() string { return e.msg }

func credClass(err error) Class {
	var ce *credError
	if errors.As(err, &ce) {
		return ce.class
	}
	return ClassLocal
}

func readAppKey(path string) (*rsa.PrivateKey, error) {
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

// appSource exchanges an app JWT for an installation token and caches the token until shortly before it expires.
type appSource struct {
	app   GitHubApp
	api   string
	clock Clock
	http  *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newAppSource(app GitHubApp, api string, clock Clock) *appSource {
	if api == "" {
		api = DefaultGitHubAPI
	}
	return &appSource{app: app, api: strings.TrimRight(api, "/"), clock: clock, http: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (a *appSource) signJWT(now time.Time) (string, error) {
	key, err := readAppKey(a.app.KeyFile)
	if err != nil {
		return "", &credError{ClassLocal, "the app key file is not usable"}
	}
	payload, _ := json.Marshal(map[string]any{"iat": now.Add(-jwtBackdate).Unix(), "exp": now.Add(jwtLife).Unix(), "iss": fmt.Sprint(a.app.AppID)})
	signing := b64([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", &credError{ClassLocal, "the app JWT could not be signed"}
	}
	return signing + "." + b64(sig), nil
}

// Token returns an installation token that stays valid for at least the refresh margin.
func (a *appSource) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock.Now()
	if a.token != "" && now.Before(a.expires.Add(-refreshMargin)) {
		return a.token, nil
	}
	jwt, err := a.signJWT(now)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", a.api, a.app.InstallationID), nil)
	if err != nil {
		return "", &credError{ClassLocal, "the token URL is not valid"}
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", &credError{ClassRemoteUnreachable, "the token endpoint did not answer"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", &credError{ClassRemoteAuth, fmt.Sprintf("the token endpoint refused the app JWT (status %d)", resp.StatusCode)}
	case resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK:
		return "", &credError{ClassRemoteUnreachable, fmt.Sprintf("the token endpoint answered status %d", resp.StatusCode)}
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if json.Unmarshal(body, &out) != nil || out.Token == "" || out.ExpiresAt.IsZero() {
		return "", &credError{ClassRemoteUnreachable, "the token endpoint answered a body that is not a token"}
	}
	a.token, a.expires = out.Token, out.ExpiresAt
	return a.token, nil
}

// remoteCreds returns the credential of a mirror for one git run and the function that removes its token file.
func (e *Engine) remoteCreds(ctx context.Context, en Entry) (gitCreds, func(), error) {
	src := e.apps[en.Name]
	if src == nil {
		return gitCreds{AskUser: en.Username, AskFile: en.CredentialFile}, func() {}, nil
	}
	tok, err := src.Token(ctx)
	if err != nil {
		return gitCreds{}, nil, err
	}
	f, err := os.CreateTemp(e.cfg.DataDir, "askpass-*")
	if err != nil {
		return gitCreds{}, nil, &credError{ClassLocal, "the token file could not be created"}
	}
	_, werr := f.WriteString(tok)
	if cerr := f.Close(); werr != nil || cerr != nil {
		_ = os.Remove(f.Name())
		return gitCreds{}, nil, &credError{ClassLocal, "the token file could not be written"}
	}
	return gitCreds{AskUser: appUsername, AskFile: f.Name()}, func() { _ = os.Remove(f.Name()) }, nil
}
