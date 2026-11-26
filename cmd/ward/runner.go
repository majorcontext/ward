package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/majorcontext/ward/internal/runner"
)

// runRunner is `ward runner`. It leases and runs checks until ctx ends and returns the exit code.
func runRunner(ctx context.Context, getenv func(string) string, stderr io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := runner.Load(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "ward runner:", err)
		return 2
	}
	if err := os.MkdirAll(cfg.Options.WorkDir, 0o755); err != nil {
		fmt.Fprintln(stderr, "ward runner:", err)
		return 1
	}
	cfg.Options.Log = log
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	waitCheckouts, err := cfg.StartCheckouts(runCtx, log)
	if err != nil {
		fmt.Fprintln(stderr, "ward runner:", err)
		return 1
	}
	defer func() { cancel(); waitCheckouts() }()
	r := runner.New(runCtx, cfg.Options)
	log.Info("ward runner started", "sandbox", cfg.SandboxKind)
	r.Pull(runCtx)
	r.Wait()
	return 0
}
