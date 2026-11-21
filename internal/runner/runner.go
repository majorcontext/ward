// Package runner is `ward runner`: a check runner that serves the hook of the runner protocol, fetches the exact tree,
// runs the argv of the check in a sandbox and posts the result to ward.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/runner/sandbox"
	"github.com/majorcontext/ward/runnerproto"
)

const (
	maxBody     = 1 << 20
	tokenLife   = 10 * time.Minute
	postTries   = 8
	postTimeout = 30 * time.Second
	logTail     = 3500
)

// Options are the parts of a Runner. Every field except Clock, Log, HTTP and WorkDir is required.
type Options struct {
	WardURL string
	// Secret returns the key of the request signature. The runner calls it for each request, so a rotated key applies at once.
	Secret func() ([]byte, error)
	// Token returns a check token for repo.
	Token func(repo string, exp time.Time) (string, error)
	// Fetch writes the files of req.Tree into dir. It must not follow a link that the tree holds.
	Fetch   func(ctx context.Context, req runnerproto.Request, dir string) error
	Sandbox sandbox.Sandbox
	// Concurrency is the most runs at one time. A request over the limit gets 503, and ward sends it again.
	Concurrency int
	WorkDir     string
	Clock       check.Clock
	Log         *slog.Logger
	HTTP        *http.Client
}

// Runner serves the hook. Start it before the first request.
type Runner struct {
	o    Options
	slot chan struct{}

	mu      sync.Mutex
	running map[string]bool
	ctx     context.Context
	wg      sync.WaitGroup
}

var _ runnerproto.Runner = (*Runner)(nil)

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
	return &Runner{o: o, slot: make(chan struct{}, o.Concurrency), running: map[string]bool{}, ctx: ctx}
}

// Wait returns when every run has ended. Call it after the context of New ends.
func (r *Runner) Wait() { r.wg.Wait() }

func (r *Runner) ServeHTTP(w http.ResponseWriter, hr *http.Request) {
	if hr.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, hr.Body, maxBody))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	secret, err := r.o.Secret()
	if err != nil {
		r.o.Log.Error("secret unreadable", "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !runnerproto.Verify(secret, body, hr.Header.Get(runnerproto.SignatureHeader)) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	req, err := runnerproto.Decode(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	dup := r.running[req.RequestID]
	r.mu.Unlock()
	if dup {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	select {
	case r.slot <- struct{}{}:
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	r.mu.Lock()
	r.running[req.RequestID] = true
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.running, req.RequestID)
			r.mu.Unlock()
			<-r.slot
		}()
		r.execute(req)
	}()
	w.WriteHeader(http.StatusAccepted)
}

func (r *Runner) execute(req runnerproto.Request) {
	res := runnerproto.Result{RequestID: req.RequestID}
	start := r.o.Clock.Now()
	status, headline, output, err := r.work(req)
	switch {
	case err != nil && r.ctx.Err() != nil:
		return
	case err != nil:
		res.Status, res.Summary = runnerproto.Fail, clip("runner error: "+err.Error())
	default:
		took := r.o.Clock.Now().Sub(start).Round(time.Millisecond)
		res.Status, res.Summary = status, clip(fmt.Sprintf("%s after %s\n%s", headline, took, output))
	}
	r.post(req, res)
}

func (r *Runner) work(req runnerproto.Request) (status runnerproto.Status, headline, output string, err error) {
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
	if err := r.o.Fetch(r.ctx, req, dir); err != nil {
		return "", "", "", fmt.Errorf("fetch tree: %w", err)
	}
	out := &tail{max: logTail}
	o, err := r.o.Sandbox.Run(r.ctx, sandbox.Spec{Dir: dir, Argv: req.Check.Run, Timeout: time.Duration(req.Check.TimeoutSeconds) * time.Second, Output: out})
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
