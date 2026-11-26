package forgeapp_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/gitcred"
	"github.com/majorcontext/ward/internal/gitcred/credtest"
	"github.com/majorcontext/ward/internal/gitcred/forgeapp"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func keyFile(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func endpoint(t *testing.T, status int, calls *atomic.Int32, now time.Time) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"token":"tok-1","expires_at":"` + now.Add(time.Hour).Format(time.RFC3339) + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConformance(t *testing.T) {
	t.Parallel()
	credtest.Run(t, credtest.Impl{New: func(t *testing.T) credtest.Subject {
		now := time.Unix(1_700_000_000, 0)
		srv := endpoint(t, http.StatusCreated, new(atomic.Int32), now)
		src := forgeapp.New(forgeapp.App{AppID: 7, InstallationID: 9, KeyFile: keyFile(t)}, srv.URL, t.TempDir(), fixedClock{now})
		return credtest.Subject{Credential: src, User: forgeapp.Username, Secret: "tok-1", Temporary: true}
	}})
}

func TestTokenIsCachedAndAFailureHasAKind(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	var calls atomic.Int32
	srv := endpoint(t, http.StatusCreated, &calls, now)
	app := forgeapp.App{AppID: 7, InstallationID: 9, KeyFile: keyFile(t)}
	src := forgeapp.New(app, srv.URL, t.TempDir(), fixedClock{now})
	for range 2 {
		_, release, err := src.Use(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if calls.Load() != 1 {
		t.Errorf("token requests = %d, want 1: the token is cached until near its expiry", calls.Load())
	}
	for name, c := range map[string]struct {
		src  *forgeapp.Source
		want gitcred.Kind
	}{
		"key file missing": {forgeapp.New(forgeapp.App{KeyFile: filepath.Join(t.TempDir(), "none")}, srv.URL, t.TempDir(), fixedClock{now}), gitcred.Local},
		"endpoint refuses": {forgeapp.New(app, endpoint(t, http.StatusUnauthorized, &calls, now).URL, t.TempDir(), fixedClock{now}), gitcred.Auth},
	} {
		_, _, err := c.src.Use(t.Context())
		var ce *gitcred.Error
		if !errors.As(err, &ce) || ce.Kind != c.want {
			t.Errorf("%s: %v, want kind %d", name, err, c.want)
		}
	}
}
