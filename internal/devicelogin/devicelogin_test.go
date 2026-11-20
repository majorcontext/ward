package devicelogin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/devicelogin"
)

func TestLoginPollsThroughPendingAndSlowDownAndKeepsTheTokenPrivate(t *testing.T) {
	t.Parallel()
	polls := 0
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	reply := func(w http.ResponseWriter, code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]string{"device_authorization_endpoint": srv.URL + "/device", "token_endpoint": srv.URL + "/token"})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]any{"device_code": "d1", "user_code": "ABCD", "verification_uri": srv.URL + "/go", "expires_in": 600, "interval": 5})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		polls++
		switch polls {
		case 1:
			reply(w, 400, map[string]string{"error": "authorization_pending"})
		case 2:
			reply(w, 400, map[string]string{"error": "slow_down"})
		default:
			reply(w, 200, map[string]any{"access_token": "opaque", "expires_in": 3600})
		}
	})
	var waits []time.Duration
	now := time.Unix(1_000_000, 0)
	prompted := ""
	tok, err := devicelogin.Login(context.Background(), devicelogin.Config{Issuer: srv.URL, ClientID: "cli", Now: func() time.Time { return now },
		Wait:   func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		Prompt: func(_, code, _ string) { prompted = code }})
	if err != nil || tok.Value != "opaque" || !tok.Expires.Equal(now.Add(time.Hour)) || prompted != "ABCD" {
		t.Fatalf("token = %+v, prompt = %q, err = %v", tok, prompted, err)
	}
	if want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}; len(waits) != 3 || waits[2] != want[2] {
		t.Errorf("waits = %v, want the third poll after slow_down at 10s", waits)
	}
	path := filepath.Join(t.TempDir(), "d", "login.json")
	if err := devicelogin.Save(path, tok); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	if _, err := devicelogin.Load(path, now.Add(2*time.Hour)); err == nil {
		t.Error("an expired token loaded")
	}
}
