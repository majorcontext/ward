// Package fake is the minimal implementation of sandbox.Sandbox for tests. It runs the argv on the host in its own
// process group. It isolates nothing, so no configuration may select it and cmd/ward never imports it.
package fake

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/majorcontext/ward/internal/procgroup"
	"github.com/majorcontext/ward/internal/runner/sandbox"
)

type Sandbox struct{}

var _ sandbox.Sandbox = Sandbox{}

func (Sandbox) Run(ctx context.Context, spec sandbox.Spec) (sandbox.Outcome, error) {
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = spec.Dir, spec.Output, spec.Output
	if spec.Cache != "" {
		cmd.Env = append(os.Environ(), sandbox.CacheEnv(spec.Cache)...)
	}
	err := procgroup.Run(runCtx, cmd, nil)
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return sandbox.Outcome{}, ctx.Err()
	case runCtx.Err() != nil:
		return sandbox.Outcome{TimedOut: true}, nil
	case errors.As(err, &exit):
		return sandbox.Outcome{ExitCode: exit.ExitCode()}, nil
	case errors.Is(err, exec.ErrNotFound):
		return sandbox.Outcome{ExitCode: 127}, nil
	}
	return sandbox.Outcome{}, err
}
