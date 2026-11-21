package push_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/dispatch/dispatchtest"
	"github.com/majorcontext/ward/internal/dispatch/push"
	"github.com/majorcontext/ward/runnerproto"
)

func TestConformance(t *testing.T) {
	secret := []byte("push-secret-0123456789")
	dispatchtest.Run(t, dispatchtest.Impl{New: func(t *testing.T, _ check.Store, repos []string) dispatchtest.Harness {
		got := map[string]chan runnerproto.Request{}
		var runners []check.Runner
		for _, repo := range repos {
			ch := make(chan runnerproto.Request, 8)
			got[repo] = ch
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				req, err := runnerproto.Decode(body)
				if !runnerproto.Verify(secret, body, r.Header.Get(runnerproto.SignatureHeader)) || err != nil {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				ch <- req
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(srv.Close)
			file := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(file, secret, 0o600); err != nil {
				t.Fatal(err)
			}
			runners = append(runners, check.Runner{Repo: repo, Mode: check.ModePush, URL: srv.URL, SecretFile: file, Env: "env1"})
		}
		return dispatchtest.Harness{Dispatcher: push.New(runners), Receive: func(ctx context.Context, repo string) (runnerproto.Request, bool) {
			select {
			case r := <-got[repo]:
				return r, true
			case <-ctx.Done():
				return runnerproto.Request{}, false
			}
		}}
	}})
}
