package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/majorcontext/ward/internal/runner"
	"github.com/majorcontext/ward/internal/runner/sandbox/fake"
	"github.com/majorcontext/ward/runnerproto"
	"github.com/majorcontext/ward/runnerproto/runnertest"
)

func TestConformance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runnertest.Run(t, runnertest.Impl{New: func(t *testing.T, e runnertest.Env) runnerproto.Runner {
		r := runner.New(ctx, runner.Options{
			WardURL: e.WardURL,
			Secret:  func() ([]byte, error) { return e.Secret, nil },
			Token: func(repo string, exp time.Time) (string, error) {
				return runnerproto.MintCheckToken(e.Kid, e.Key, repo, "test", exp)
			},
			Fetch:       func(context.Context, runnerproto.Request, string) error { return nil },
			Sandbox:     fake.Sandbox{},
			Concurrency: 4,
			WorkDir:     t.TempDir(),
		})
		t.Cleanup(func() { cancel(); r.Wait() })
		return r
	}})
}
