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
	// Cache is an optional host directory that persists across runs. The argv sees it as read-write at the path in the
	// variable WARD_CACHE_DIR, which CacheEnv sets with the Go and XDG cache variables. It never holds the tree.
	Cache string
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

// CacheMount is the path at which a container sandbox shows Spec.Cache.
const CacheMount = "/cache"

// CacheEnv returns the variables that point the caches of the argv at the cache directory dir as the argv sees it.
func CacheEnv(dir string) []string {
	return []string{"WARD_CACHE_DIR=" + dir, "GOCACHE=" + dir + "/go-build", "GOMODCACHE=" + dir + "/go-mod", "XDG_CACHE_HOME=" + dir + "/xdg"}
}
