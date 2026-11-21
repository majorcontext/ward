// Package sandbox is the seam between a check runner and the place where check code runs. A Sandbox is the only way that
// the runner executes the argv of a check, and every implementation must isolate that argv from the host.
package sandbox

import (
	"context"
	"io"
	"time"
)

// Spec is one run.
type Spec struct {
	// Dir is a host directory that holds the tree. The argv runs with the root of Dir as its working directory and may write there.
	Dir  string
	Argv []string
	// Timeout is the longest run time. When it passes, the sandbox kills every process that the argv started.
	Timeout time.Duration
	// Output receives the standard output and the standard error of the argv, interleaved.
	Output io.Writer
}

// Outcome is how a run ended. A non-zero ExitCode is not an error: the check failed.
type Outcome struct {
	ExitCode int
	TimedOut bool
}

// Sandbox runs a check. Run returns an error only when the sandbox itself fails or when ctx ends before the argv does.
// A missing argv[0] ends with ExitCode 127. When Run returns, no process of the run is left.
type Sandbox interface {
	Run(ctx context.Context, spec Spec) (Outcome, error)
}
