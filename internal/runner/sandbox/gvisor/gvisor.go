// Package gvisor runs a check in an OCI container under the gVisor runtime runsc, through a container engine.
package gvisor

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/majorcontext/ward/internal/runner/sandbox"
	"github.com/majorcontext/ward/internal/runner/sandbox/container"
)

// Runtime is the OCI runtime name of gVisor.
const Runtime = "runsc"

// Sandbox is a container sandbox whose runtime is always runsc.
type Sandbox struct{ inner *container.Sandbox }

var _ sandbox.Sandbox = (*Sandbox)(nil)

// Available reports whether the runsc binary is on the path.
func Available() error {
	if _, err := exec.LookPath(Runtime); err != nil {
		return fmt.Errorf("gvisor sandbox: %w", err)
	}
	return nil
}

// New returns a sandbox. It fails when runsc is missing, and it never falls back to another runtime.
func New(cfg container.Config) (*Sandbox, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	cfg.Runtime = Runtime
	inner, err := container.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Sandbox{inner: inner}, nil
}

func (s *Sandbox) Run(ctx context.Context, spec sandbox.Spec) (sandbox.Outcome, error) {
	return s.inner.Run(ctx, spec)
}
