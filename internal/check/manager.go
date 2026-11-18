package check

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	maxSummary    = 4096
	maxLogURL     = 2048
	resendMargin  = 10 * time.Minute
	postTimeout   = 30 * time.Second
	signatureName = "Ward-Signature"
)

var (
	// ErrInvalid means that a posted result breaks a rule of the body.
	ErrInvalid = errors.New("check: invalid result")
	// ErrUnknownRequest means that no request has the posted request_id.
	ErrUnknownRequest = errors.New("check: unknown request")
	// ErrOtherRepo means that the request belongs to another repository.
	ErrOtherRepo = errors.New("check: request of another repository")
)

// State is the state of one check for one tree.
type State string

const (
	Pending State = "pending"
	Pass    State = "pass"
	Fail    State = "fail"
	Flaky   State = "flaky"
)

// Verdict is the state of one check, and the failing result when the state is Fail.
type Verdict struct {
	State  State
	Result Result
}

// Posted is the body that a runner posts.
type Posted struct {
	RequestID string `json:"request_id"`
	Status    Status `json:"status"`
	Summary   string `json:"summary"`
	LogURL    string `json:"log_url"`
}

type wireRequest struct {
	RequestID string `json:"request_id"`
	Repo      string `json:"repo"`
	Tree      string `json:"tree"`
	Commit    string `json:"commit"`
	Check     Check  `json:"check"`
	CheckHash string `json:"check_hash"`
	Env       string `json:"env"`
}

// Manager keeps the results and sends the check requests. It has no poll: a result wakes the waiters.
type Manager struct {
	store   Store
	runners map[string]Runner
	clock   Clock
	log     *slog.Logger
	http    *http.Client

	mu      sync.Mutex
	changed chan struct{}
	ctx     context.Context
	wg      sync.WaitGroup
}

// New returns a manager. A nil clock means the wall clock.
func New(store Store, runners []Runner, clock Clock, log *slog.Logger) *Manager {
	if clock == nil {
		clock = RealClock()
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Manager{store: store, runners: map[string]Runner{}, clock: clock, log: log, changed: make(chan struct{})}
	for _, r := range runners {
		m.runners[r.Repo] = r
	}
	m.http = &http.Client{Timeout: postTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return m
}

// Start resumes every open request. The deliveries stop when ctx ends.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	open, err := m.store.OpenRequests(ctx)
	if err != nil {
		return err
	}
	for _, r := range open {
		m.spawn(r)
	}
	return nil
}

// Wait returns when every delivery has stopped. Call it after the context of Start ends.
func (m *Manager) Wait() { m.wg.Wait() }

// Changed returns a channel that closes at the next result. Take it before reading the state that the result changes.
func (m *Manager) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

func (m *Manager) broadcast() {
	m.mu.Lock()
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
}

// Runner returns the runner of a repository.
func (m *Manager) Runner(repo string) (Runner, bool) {
	r, ok := m.runners[repo]
	return r, ok
}

// Mark returns the position after the newest result. A fail counts for a land only when it is after its mark.
func (m *Manager) Mark(ctx context.Context) (int64, error) { return m.store.MaxResultID(ctx) }

// Verdict reads the state of check c for tree. A pass is final. A fail counts only when it is after mark.
func (m *Manager) Verdict(ctx context.Context, repo, tree string, c Check, mark int64) (Verdict, error) {
	r, ok := m.runners[repo]
	if !ok {
		return Verdict{}, ErrNoRunner
	}
	results, err := m.store.Results(ctx, repo, tree, c.Hash(), r.Env)
	if err != nil {
		return Verdict{}, err
	}
	var pass, fail bool
	var failed Result
	for _, res := range results {
		if res.Status == StatusPass {
			pass = true
			continue
		}
		fail = true
		if res.ID > mark && failed.ID == 0 {
			failed = res
		}
	}
	switch {
	case pass && fail:
		return Verdict{State: Flaky}, nil
	case pass:
		return Verdict{State: Pass}, nil
	case failed.ID != 0:
		return Verdict{State: Fail, Result: failed}, nil
	}
	return Verdict{State: Pending}, nil
}

// Ensure makes sure that a request for check c of tree is open, and starts its delivery when it opens one.
func (m *Manager) Ensure(ctx context.Context, repo, tree, commit string, c Check) error {
	r, ok := m.runners[repo]
	if !ok {
		return ErrNoRunner
	}
	id, err := newID()
	if err != nil {
		return err
	}
	req, created, err := m.store.OpenRequest(ctx, Request{ID: id, Repo: repo, Tree: tree, Commit: commit, Check: c, Hash: c.Hash(), Env: r.Env, Open: true})
	if err != nil {
		return err
	}
	if created {
		m.spawn(req)
	}
	return nil
}

// Post records a result that a runner posted for a repository. A second result for a request changes nothing.
func (m *Manager) Post(ctx context.Context, repo string, p Posted) error {
	if (p.Status != StatusPass && p.Status != StatusFail) || len(p.Summary) > maxSummary || len(p.LogURL) > maxLogURL {
		return ErrInvalid
	}
	req, ok, err := m.store.Request(ctx, p.RequestID)
	switch {
	case err != nil:
		return err
	case !ok:
		return ErrUnknownRequest
	case req.Repo != repo:
		return ErrOtherRepo
	}
	added, err := m.store.AddResult(ctx, Result{RequestID: req.ID, Repo: req.Repo, Tree: req.Tree, Hash: req.Hash, Env: req.Env, Status: p.Status, Summary: p.Summary, LogURL: p.LogURL})
	if err != nil {
		return err
	}
	if added {
		m.broadcast()
	}
	return nil
}

func (m *Manager) spawn(r Request) {
	m.wg.Add(1)
	go m.deliver(m.ctx, r)
}

// deliver sends the request until a runner accepts it, waits for its result, and sends a new request when the result is late.
func (m *Manager) deliver(ctx context.Context, req Request) {
	defer m.wg.Done()
	for {
		if req.AcceptedAt.IsZero() && !m.send(ctx, &req) {
			return
		}
		next, ok := m.awaitResult(ctx, req)
		if !ok {
			return
		}
		req = next
	}
}

// send posts req with the backoff until a runner accepts it. It returns false when the request ended or ctx ended.
func (m *Manager) send(ctx context.Context, req *Request) bool {
	for attempt := 1; ; attempt++ {
		cur, ok, err := m.store.Request(ctx, req.ID)
		if err == nil && ok && !cur.Open {
			return false
		}
		if err = m.post(ctx, *req); err == nil {
			now := m.clock.Now()
			if err = m.store.SetAccepted(ctx, req.ID, now); err == nil {
				req.AcceptedAt = now
				return true
			}
		}
		if ctx.Err() != nil {
			return false
		}
		m.log.Warn("check request not accepted", "request_id", req.ID, "attempt", attempt)
		select {
		case <-m.clock.After(backoff(attempt)):
		case <-ctx.Done():
			return false
		}
	}
}

// awaitResult waits until the request ends, or until it is late, and then replaces it.
func (m *Manager) awaitResult(ctx context.Context, req Request) (Request, bool) {
	late := m.clock.After(req.AcceptedAt.Add(time.Duration(req.Check.TimeoutSeconds)*time.Second + resendMargin).Sub(m.clock.Now()))
	for {
		changed := m.Changed()
		cur, ok, err := m.store.Request(ctx, req.ID)
		if err != nil || !ok || !cur.Open {
			return Request{}, false
		}
		select {
		case <-changed:
		case <-late:
			next, ok, err := m.resend(ctx, req)
			return next, err == nil && ok
		case <-ctx.Done():
			return Request{}, false
		}
	}
}

func (m *Manager) resend(ctx context.Context, old Request) (Request, bool, error) {
	id, err := newID()
	if err != nil {
		return Request{}, false, err
	}
	next := old
	next.ID, next.AcceptedAt, next.Open = id, time.Time{}, true
	ok, err := m.store.Supersede(ctx, old.ID, next)
	return next, ok, err
}

func (m *Manager) post(ctx context.Context, req Request) error {
	r, ok := m.runners[req.Repo]
	if !ok {
		return ErrNoRunner
	}
	secret, err := r.secret()
	if err != nil {
		return err
	}
	body, err := json.Marshal(wireRequest{RequestID: req.ID, Repo: req.Repo, Tree: req.Tree, Commit: req.Commit, Check: req.Check, CheckHash: req.Hash, Env: req.Env})
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set(signatureName, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := m.http.Do(hr)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("runner answered %d", resp.StatusCode)
	}
	return nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
