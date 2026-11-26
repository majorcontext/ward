// Package runner is `ward runner`: a check runner that leases check requests from ward, fetches the exact tree,
// runs the argv of the check in a sandbox and posts the result to ward.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/runner/sandbox"
	"github.com/majorcontext/ward/runnerproto"
)

const (
	tokenLife   = 10 * time.Minute
	postTries   = 8
	postTimeout = 30 * time.Second
	logTail     = 3500
)

// Options are the parts of a Runner. Every field except Clock, Log, HTTP and WorkDir is required.
type Options struct {
	WardURL string
	// Token returns a check token for repo.
	Token func(repo string, exp time.Time) (string, error)
	// Fetch writes the files of req.Tree into dir. It must not follow a link that the tree holds.
	Fetch   func(ctx context.Context, req runnerproto.Request, dir string) error
	Sandbox sandbox.Sandbox
	// Concurrency is the most leases that the runner holds at one time.
	Concurrency int
	WorkDir     string
	// CacheDir is the root of the durable caches. Each repository and env gets a directory under it, so a check sees only
	// the cache of its own repository. Empty means no cache.
	CacheDir string
	// RunnerToken returns a runner token. Pull reports the isolation of Sandbox with it.
	RunnerToken func(exp time.Time) (string, error)
	Clock       check.Clock
	Log         *slog.Logger
	HTTP        *http.Client
}

// Runner leases check requests and runs them.
type Runner struct {
	o    Options
	slot chan struct{}
	ctx  context.Context
	wg   sync.WaitGroup
}

// New returns a runner. The runs stop when ctx ends.
func New(ctx context.Context, o Options) *Runner {
	if o.Clock == nil {
		o.Clock = check.RealClock()
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: postTimeout}
	}
	if o.Concurrency < 1 {
		o.Concurrency = 1
	}
	return &Runner{o: o, slot: make(chan struct{}, o.Concurrency), ctx: ctx}
}

// Wait returns when every run has ended. Call it after the context of New ends.
func (r *Runner) Wait() { r.wg.Wait() }

// execute runs req and posts the result. It posts nothing when ctx ends first: ward leases the request again.
func (r *Runner) execute(ctx context.Context, req runnerproto.Request) {
	res := runnerproto.Result{RequestID: req.RequestID}
	start := r.o.Clock.Now()
	status, headline, output, err := r.work(ctx, req)
	switch {
	case err != nil && ctx.Err() != nil:
		return
	case err != nil:
		res.Status, res.Summary = runnerproto.Fail, clip("runner error: "+err.Error())
	default:
		took := r.o.Clock.Now().Sub(start).Round(time.Millisecond)
		res.Status, res.Summary = status, clip(fmt.Sprintf("%s after %s\n%s", headline, took, output))
	}
	r.post(req, res)
}

func (r *Runner) work(ctx context.Context, req runnerproto.Request) (status runnerproto.Status, headline, output string, err error) {
	dir, err := os.MkdirTemp(r.o.WorkDir, "check-")
	if err != nil {
		return "", "", "", err
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			r.o.Log.Error("checkout not removed", "dir", dir, "error", err.Error())
		}
	}()
	if err := os.Chmod(dir, 0o777); err != nil {
		return "", "", "", err
	}
	if err := r.o.Fetch(ctx, req, dir); err != nil {
		return "", "", "", fmt.Errorf("fetch tree: %w", err)
	}
	cache, err := r.cacheDir(req)
	if err != nil {
		return "", "", "", err
	}
	out := &tail{max: logTail}
	o, err := r.o.Sandbox.Run(ctx, sandbox.Spec{Dir: dir, Cache: cache, Argv: req.Check.Run, Timeout: time.Duration(req.Check.TimeoutSeconds) * time.Second, Output: out})
	switch {
	case err != nil:
		return "", "", "", err
	case o.TimedOut:
		return runnerproto.Fail, fmt.Sprintf("timed out (%ds)", req.Check.TimeoutSeconds), out.String(), nil
	case o.ExitCode != 0:
		return runnerproto.Fail, fmt.Sprintf("exit code %d", o.ExitCode), out.String(), nil
	}
	return runnerproto.Pass, "exit code 0", out.String(), nil
}

// cacheDir returns the cache directory of the repository and env of req, and makes it. Two repositories never share one.
func (r *Runner) cacheDir(req runnerproto.Request) (string, error) {
	if r.o.CacheDir == "" {
		return "", nil
	}
	dir := filepath.Join(r.o.CacheDir, digest(req.Repo), digest(req.Env))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	return dir, os.Chmod(dir, 0o777)
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func (r *Runner) post(req runnerproto.Request, res runnerproto.Result) {
	for attempt := 1; attempt <= postTries; attempt++ {
		tok, err := r.o.Token(req.Repo, r.o.Clock.Now().Add(tokenLife))
		if err == nil {
			err = runnerproto.PostResult(r.ctx, r.o.HTTP, r.o.WardURL, tok, res)
		}
		var se *runnerproto.StatusError
		if err == nil {
			return
		}
		if errors.As(err, &se) && se.Final() {
			r.o.Log.Error("result refused", "request_id", req.RequestID, "status", se.Code)
			return
		}
		r.o.Log.Warn("result not posted", "request_id", req.RequestID, "attempt", attempt, "error", err.Error())
		select {
		case <-r.o.Clock.After(min(time.Second<<attempt, time.Minute)):
		case <-r.ctx.Done():
			return
		}
	}
}

func clip(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= runnerproto.MaxSummary {
		return s
	}
	s = s[:runnerproto.MaxSummary]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
