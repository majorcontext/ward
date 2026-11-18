// Package config reads the configuration of ward from the environment.
package config

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/land"
	"github.com/majorcontext/ward/internal/mirror"
	"github.com/majorcontext/ward/internal/token"
)

const (
	minSecret = 16
	maxSecret = 256
)

// Config is the configuration of the service.
type Config struct {
	Listen      string
	StateFile   string
	Mirror      mirror.Config
	Land        land.Config
	TrustedKeys []token.Key
	OTLP        bool
}

// Load reads the configuration from getenv. It refuses a value that the specification calls an error.
func Load(getenv func(string) string) (Config, error) {
	c := Config{Listen: ":8080", OTLP: getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""}
	if v := getenv("WARD_LISTEN"); v != "" {
		c.Listen = v
	}
	for _, name := range []string{"WARD_DATA_DIR", "WARD_MOTTE_URL", "WARD_MOTTE_KID", "WARD_MOTTE_KEY_FILE"} {
		if getenv(name) == "" {
			return c, fmt.Errorf("%s is required", name)
		}
	}
	var err error
	if c.Mirror.DataDir, err = filepath.Abs(getenv("WARD_DATA_DIR")); err != nil {
		return c, fmt.Errorf("WARD_DATA_DIR: %w", err)
	}
	c.StateFile = filepath.Join(c.Mirror.DataDir, "state.db")
	if v := getenv("WARD_STATE_FILE"); v != "" {
		if c.StateFile, err = filepath.Abs(v); err != nil {
			return c, fmt.Errorf("WARD_STATE_FILE: %w", err)
		}
	}
	key, err := ReadPrivateKey(getenv("WARD_MOTTE_KEY_FILE"))
	if err != nil {
		return c, fmt.Errorf("WARD_MOTTE_KEY_FILE: %w", err)
	}
	c.Mirror.Motte = mirror.MotteConfig{URL: getenv("WARD_MOTTE_URL"), Kid: getenv("WARD_MOTTE_KID"), Key: key}
	c.Land = land.Config{URL: c.Mirror.Motte.URL, Kid: c.Mirror.Motte.Kid, Key: key}
	if v := getenv("WARD_GITHUB_API_URL"); v != "" {
		if u, err := url.Parse(v); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return c, errors.New("WARD_GITHUB_API_URL must be an http or https URL")
		}
		c.Mirror.GitHubAPI = v
	}
	if c.TrustedKeys, err = parseTrustedKeys(getenv("WARD_TRUSTED_KEYS")); err != nil {
		return c, fmt.Errorf("WARD_TRUSTED_KEYS: %w", err)
	}
	if raw := getenv("WARD_MIRRORS"); raw != "" {
		if c.Mirror.Mirrors, err = mirror.ParseEntries([]byte(raw)); err != nil {
			return c, fmt.Errorf("WARD_MIRRORS: %w", err)
		}
	}
	if err := c.loadMirrorEnv(getenv); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Config) loadMirrorEnv(getenv func(string) string) error {
	if len(c.Mirror.Mirrors) > 0 {
		if getenv("WARD_PUBLIC_URL") == "" || getenv("WARD_HOOK_SECRET_FILE") == "" {
			return errors.New("WARD_PUBLIC_URL and WARD_HOOK_SECRET_FILE are required when a mirror is configured")
		}
		c.Mirror.PublicURL = strings.TrimRight(getenv("WARD_PUBLIC_URL"), "/")
		raw, err := os.ReadFile(getenv("WARD_HOOK_SECRET_FILE"))
		if err != nil {
			return errors.New("WARD_HOOK_SECRET_FILE is unreadable")
		}
		c.Mirror.HookSecret = bytes.TrimRight(raw, "\r\n")
		if n := len(c.Mirror.HookSecret); n < minSecret || n > maxSecret {
			return fmt.Errorf("WARD_HOOK_SECRET_FILE must hold %d to %d bytes", minSecret, maxSecret)
		}
	}
	if v := getenv("WARD_CLONE_DISK_BUDGET"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return errors.New("WARD_CLONE_DISK_BUDGET must be a positive number of bytes")
		}
		c.Mirror.DiskBudget = n
	}
	if v := getenv("WARD_CLONE_IDLE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return errors.New("WARD_CLONE_IDLE_TTL must be a positive Go duration")
		}
		c.Mirror.IdleTTL = d
	}
	return nil
}

// ReadPrivateKey reads an Ed25519 private key file: the standard base64 of the 64-byte key or of the 32-byte seed.
func ReadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("unreadable")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	switch {
	case err != nil:
		return nil, errors.New("not standard base64")
	case len(b) == ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case len(b) == ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	}
	return nil, errors.New("not an Ed25519 private key or seed")
}

func parseTrustedKeys(raw string) ([]token.Key, error) {
	if raw == "" {
		return nil, nil
	}
	var in []struct {
		Kid    string   `json:"kid"`
		Key    string   `json:"key"`
		Scopes []string `json:"scopes"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, err
	}
	keys := make([]token.Key, 0, len(in))
	for _, k := range in {
		pub, err := base64.StdEncoding.DecodeString(k.Key)
		if k.Kid == "" || err != nil || len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("key %q is not a standard base64 Ed25519 public key", k.Kid)
		}
		tk := token.Key{Kid: k.Kid, Public: pub}
		for _, s := range k.Scopes {
			if s != string(token.ScopeRead) && s != string(token.ScopeOperator) && s != string(token.ScopeLand) {
				return nil, fmt.Errorf("key %q has the unknown scope %q", k.Kid, s)
			}
			tk.Scopes = append(tk.Scopes, token.Scope(s))
		}
		keys = append(keys, tk)
	}
	return keys, nil
}
