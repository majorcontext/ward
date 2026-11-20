// Package devicelogin signs a person in with the OAuth device authorization grant (RFC 8628) against any OIDC
// issuer, and keeps the token in a file that only the user reads.
package devicelogin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is one login.
type Config struct {
	Issuer   string
	ClientID string
	// Scope defaults to "openid email".
	Scope  string
	Client *http.Client
	Now    func() time.Time
	// Wait pauses for d between polls, or returns the error of ctx. It defaults to a timer.
	Wait func(ctx context.Context, d time.Duration) error
	// Prompt tells the person where to go and what to enter.
	Prompt func(verificationURI, userCode, verificationURIComplete string)
}

// Token is the credential that a login gave. It is the ID token when the issuer returns one, and the access token
// otherwise: the ID token is a JWT whose audience is the client.
type Token struct {
	Issuer  string    `json:"issuer"`
	Value   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// Login runs the flow and returns the token.
func Login(ctx context.Context, cfg Config) (Token, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return Token{}, errors.New("an issuer and a client ID are required")
	}
	if cfg.Scope == "" {
		cfg.Scope = "openid email"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	var doc struct {
		DeviceEndpoint string `json:"device_authorization_endpoint"`
		TokenEndpoint  string `json:"token_endpoint"`
	}
	if err := getJSON(ctx, cfg.Client, strings.TrimSuffix(cfg.Issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil || doc.DeviceEndpoint == "" || doc.TokenEndpoint == "" {
		return Token{}, fmt.Errorf("the issuer does not offer the device flow: %v", err)
	}
	var dev struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if _, err := post(ctx, cfg.Client, doc.DeviceEndpoint, url.Values{"client_id": {cfg.ClientID}, "scope": {cfg.Scope}}, &dev); err != nil || dev.DeviceCode == "" {
		return Token{}, fmt.Errorf("device authorization: %v", err)
	}
	if cfg.Prompt != nil {
		cfg.Prompt(dev.VerificationURI, dev.UserCode, dev.VerificationURIComplete)
	}
	interval := time.Duration(max(dev.Interval, 5)) * time.Second
	deadline := cfg.Now().Add(time.Duration(max(dev.ExpiresIn, 60)) * time.Second)
	for cfg.Now().Before(deadline) {
		if err := cfg.Wait(ctx, interval); err != nil {
			return Token{}, err
		}
		var tok struct {
			IDToken     string `json:"id_token"`
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
			Error       string `json:"error"`
		}
		if _, err := post(ctx, cfg.Client, doc.TokenEndpoint, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {dev.DeviceCode}, "client_id": {cfg.ClientID}}, &tok); err != nil && tok.Error == "" {
			return Token{}, err
		}
		switch tok.Error {
		case "":
			return newToken(cfg, tok.IDToken, tok.AccessToken, tok.ExpiresIn)
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return Token{}, fmt.Errorf("login refused: %s", tok.Error)
		}
	}
	return Token{}, errors.New("login timed out: the code expired")
}

func newToken(cfg Config, id, access string, expiresIn int) (Token, error) {
	t := Token{Issuer: cfg.Issuer, Value: id, Expires: cfg.Now().Add(time.Duration(expiresIn) * time.Second)}
	if id == "" {
		t.Value = access
	} else if exp := jwtExpiry(id); !exp.IsZero() {
		t.Expires = exp
	}
	if t.Value == "" {
		return Token{}, errors.New("the issuer returned no token")
	}
	return t, nil
}

func jwtExpiry(jwt string) time.Time {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct{ Exp int64 }
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.Exp, 0)
}

func getJSON(ctx context.Context, c *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// post decodes the body of any status, because an OAuth error is a 400 with a JSON body.
func post(ctx context.Context, c *http.Client, u string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// Save writes t to path with mode 0600, in a directory of mode 0700.
func Save(path string, t Token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads the token at path. It refuses a token that is expired at now.
func Load(path string, now time.Time) (Token, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Token{}, err
	}
	var t Token
	if err := json.Unmarshal(raw, &t); err != nil || t.Value == "" {
		return Token{}, errors.New("the saved token is unreadable: log in again")
	}
	if !now.Before(t.Expires) {
		return Token{}, errors.New("the saved token expired: log in again")
	}
	return t, nil
}

// DefaultPath is the file of the token of tool, under the user configuration directory.
func DefaultPath(tool string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tool, "login.json"), nil
}
