package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/majorcontext/ward/internal/runner"
)

// runRunner is `ward runner`. It serves the hook of a check runner until ctx ends and returns the exit code.
func runRunner(ctx context.Context, getenv func(string) string, ln net.Listener, stderr io.Writer) int {
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
	if cfg.Pull {
		log.Info("ward runner started", "mode", "pull", "sandbox", cfg.SandboxKind)
		r.Pull(runCtx)
		r.Wait()
		return 0
	}
	if ln == nil {
		if ln, err = net.Listen("tcp", cfg.Listen); err != nil {
			fmt.Fprintln(stderr, "ward runner:", err)
			return 1
		}
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/checks", r)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Info("ward runner started", "sandbox", cfg.SandboxKind, "listen", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	cancel()
	sctx, stop := context.WithTimeout(context.Background(), shutdownGrace)
	defer stop()
	_ = srv.Shutdown(sctx)
	r.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, "ward runner:", err)
		return 1
	}
	return 0
}
