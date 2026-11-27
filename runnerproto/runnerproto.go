// Package runnerproto is the wire protocol between ward and a check runner: the lease that carries a check request, the
// result that a runner posts and the tokens that authorize both. ward, `ward runner` and a runner of
// another author share this definition. runner.schema.json describes the JSON bodies.
package runnerproto

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/token"
)

const (
	// ResultsPath is the route of ward that takes a result, with a check token as the bearer credential.
	ResultsPath = "/v1/results"
	MaxSummary  = 4096
	MaxLogURL   = 2048
)

// Check is one required check. The runner runs Run at the root of the tree.
type Check struct {
	Name           string   `json:"name"`
	Run            []string `json:"run"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// Hash is the SHA-256, in lowercase hex, of the compact JSON object of the check.
func (c Check) Hash() string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(c)
	sum := sha256.Sum256(bytes.TrimRight(b.Bytes(), "\n"))
	return hex.EncodeToString(sum[:])
}

// Request is the check request that a lease carries. Commit is a commit of motte whose tree is Tree.
type Request struct {
	RequestID string `json:"request_id"`
	Repo      string `json:"repo"`
	Tree      string `json:"tree"`
	Commit    string `json:"commit"`
	Check     Check  `json:"check"`
	CheckHash string `json:"check_hash"`
	Env       string `json:"env"`
	// Base is the commit that the change under check starts from, and BaseTree is its tree. They are empty when ward does not send them.
	Base     string `json:"base,omitempty"`
	BaseTree string `json:"base_tree,omitempty"`
}

// Status is the outcome that a runner reports.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
)

// Result is the body that a runner posts to ResultsPath.
type Result struct {
	RequestID string `json:"request_id"`
	Status    Status `json:"status"`
	Summary   string `json:"summary"`
	LogURL    string `json:"log_url"`
}

// Decode reads a request body. It checks that check_hash is the hash of check.
func Decode(body []byte) (Request, error) {
	var r Request
	if err := json.Unmarshal(body, &r); err != nil {
		return Request{}, fmt.Errorf("request: %w", err)
	}
	if r.RequestID == "" || r.Repo == "" || r.Tree == "" || r.Commit == "" || len(r.Check.Run) == 0 {
		return Request{}, errors.New("request: request_id, repo, tree, commit and check.run are required")
	}
	if r.CheckHash != r.Check.Hash() {
		return Request{}, errors.New("request: check_hash is not the hash of check")
	}
	return r, nil
}

// MintCheckToken signs a check token for repo. A runner sends it as the bearer credential of PostResult.
func MintCheckToken(kid string, key ed25519.PrivateKey, repo, subject string, exp time.Time) (string, error) {
	return token.Signer{Kid: kid, Key: key}.Mint(token.Claims{Scope: token.ScopeCheck, Repo: repo, Subject: subject, Exp: exp.Unix()})
}

// StatusError is a refusal of ward. A 4xx code other than 429 is final: the runner must not repeat the post.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("ward answered %d", e.Code) }

// Final reports whether the post cannot succeed on a repeat.
func (e *StatusError) Final() bool {
	return e.Code >= 400 && e.Code < 500 && e.Code != http.StatusTooManyRequests
}

// PostResult posts res to the ward at baseURL with a check token.
func PostResult(ctx context.Context, c *http.Client, baseURL, tok string, res Result) error {
	body, err := json.Marshal(res)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+ResultsPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return &StatusError{Code: resp.StatusCode}
	}
	return nil
}
