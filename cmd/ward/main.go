// Command ward is the forge service on top of motte.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/config"
	"github.com/majorcontext/ward/internal/identity"
	"github.com/majorcontext/ward/internal/identity/oidc"
	"github.com/majorcontext/ward/internal/identity/usertoken"
	"github.com/majorcontext/ward/internal/land"
	"github.com/majorcontext/ward/internal/mirror"
	mirrorsqlite "github.com/majorcontext/ward/internal/mirror/sqlite"
	"github.com/majorcontext/ward/internal/server"
	"github.com/majorcontext/ward/internal/telemetry"
	"github.com/majorcontext/ward/internal/token"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const shutdownGrace = 10 * time.Second

func main() {
	if mirror.MaybeAskpass(os.Args[1:], os.Stdout) {
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println("ward", version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "land" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		code := runLand(ctx, os.Args[2:], os.Getenv, time.Now, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	if len(os.Args) > 1 && (os.Args[1] == "token" || os.Args[1] == "login") {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		run := runToken
		if os.Args[1] == "login" {
			run = runLogin
		}
		code := run(ctx, os.Args[2:], os.Getenv, time.Now, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	if len(os.Args) > 1 && os.Args[1] == "check" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		code := runLocalCheck(ctx, os.Args[2:], time.Now, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: ward [version | land [--ref REF] [--delete-branch] REPO BRANCH | token create|list|revoke | login [--print] | check [--dir PATH] [--only NAME]]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(ctx, os.Getenv, nil, mirror.Options{Logger: logger}, nil); err != nil {
		logger.Error("ward stopped", "error", err.Error())
		os.Exit(1)
	}
}

// run serves until ctx ends. A nil listener means that run listens on WARD_LISTEN. A non-nil adjust edits the loaded
// configuration: a test points a mirror at a plain HTTP remote with it.
func run(ctx context.Context, getenv func(string) string, ln net.Listener, opts mirror.Options, adjust func(*config.Config)) error {
	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cfg.Mirror.Askpass = exe
	if adjust != nil {
		adjust(&cfg)
	}
	if err := os.MkdirAll(cfg.Mirror.DataDir, 0o755); err != nil {
		return err
	}
	shutdownTraces, err := telemetry.Setup(ctx, cfg.OTLP)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTraces(context.Background()) }()
	store, err := mirrorsqlite.Open(ctx, cfg.StateFile)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	var clock check.Clock
	if opts.Clock != nil {
		clock = opts.Clock
	}
	checks := check.New(store, cfg.Runners, clock, opts.Logger)
	lands := land.New(cfg.Land, store, checks, clock, opts.Logger)
	if err := checks.Start(ctx); err != nil {
		return err
	}
	if err := lands.Start(ctx); err != nil {
		return err
	}
	defer func() { cancelAll(); lands.Wait(); checks.Wait() }()
	deps := server.Deps{Store: store, Verifier: token.NewVerifier(cfg.TrustedKeys...), Logger: opts.Logger, Land: lands, Checks: checks, UserTokens: store, Now: time.Now}
	var viaOIDC identity.Authenticator
	if cfg.OIDC != nil {
		if viaOIDC, err = oidc.New(*cfg.OIDC); err != nil {
			return err
		}
	}
	deps.Human = identity.Chain(viaOIDC, usertoken.Authenticator{Store: store, Now: deps.Now})
	var engine *mirror.Engine
	if len(cfg.Mirror.Mirrors) > 0 {
		engine = mirror.New(cfg.Mirror, store, opts)
		deps.Engine, deps.Registry = engine, engine.Registry()
	}
	if ln == nil {
		if ln, err = net.Listen("tcp", cfg.Listen); err != nil {
			return err
		}
	}
	srv := &http.Server{Handler: server.New(deps), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" {
			errc <- srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
			return
		}
		errc <- srv.Serve(ln)
	}()
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		if engine != nil {
			_ = engine.Run(ctx)
		}
	}()
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	cancelAll()
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = srv.Shutdown(sctx)
	<-engineDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
