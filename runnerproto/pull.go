package runnerproto

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/token"
)

const (
	// LeasesPath is the route that a pull runner polls for a lease. The bearer credential is a runner token.
	LeasesPath = "/v1/runner/leases"
	// LeaseWait is the longest time that ward holds a poll open before it answers 204.
	LeaseWait = 20 * time.Second
)

// RenewPath is the route that extends the lease with the given ID.
func RenewPath(leaseID string) string { return LeasesPath + "/" + url.PathEscape(leaseID) + "/renew" }

// Isolation is the strength of the sandbox that a runner reports. A larger rank isolates more.
type Isolation string

const (
	IsolationContainer Isolation = "container"
	IsolationGVisor    Isolation = "gvisor"
)

// Rank orders isolation levels. An unknown level has rank 0.
func (i Isolation) Rank() int {
	switch i {
	case IsolationContainer:
		return 1
	case IsolationGVisor:
		return 2
	}
	return 0
}

// AtLeast reports whether i is a known level that isolates at least as much as min.
func (i Isolation) AtLeast(min Isolation) bool { return i.Rank() > 0 && i.Rank() >= min.Rank() }

// LeaseRequest is the body of a poll. Isolation is the sandbox that the runner uses for every check.
type LeaseRequest struct {
	Isolation Isolation `json:"isolation"`
}

// Lease is the answer to a poll: one check request that the runner holds for TTLSeconds, and renews until it posts a result.
type Lease struct {
	LeaseID    string  `json:"lease_id"`
	TTLSeconds int     `json:"ttl_seconds"`
	Request    Request `json:"request"`
}

// Renewal is the answer to a renewal.
type Renewal struct {
	TTLSeconds int `json:"ttl_seconds"`
}

// MintRunnerToken signs a runner token. A pull runner sends it as the bearer credential of a poll and of a renewal.
func MintRunnerToken(kid string, key ed25519.PrivateKey, subject string, exp time.Time) (string, error) {
	return token.Signer{Kid: kid, Key: key}.Mint(token.Claims{Scope: token.ScopeRunner, Subject: subject, Exp: exp.Unix()})
}

func postJSON(ctx context.Context, c *http.Client, baseURL, path, tok string, in, out any) (int, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, &StatusError{Code: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("lease answer: %w", err)
	}
	return resp.StatusCode, nil
}

// PollLease asks ward for a lease. It returns nil with no error when ward has no work after LeaseWait.
func PollLease(ctx context.Context, c *http.Client, baseURL, tok string, iso Isolation) (*Lease, error) {
	var l Lease
	code, err := postJSON(ctx, c, baseURL, LeasesPath, tok, LeaseRequest{Isolation: iso}, &l)
	if err != nil || code == http.StatusNoContent {
		return nil, err
	}
	return &l, nil
}

// RenewLease extends a lease. A final StatusError means that the runner lost the lease and must stop the check.
func RenewLease(ctx context.Context, c *http.Client, baseURL, tok, leaseID string) (Renewal, error) {
	var r Renewal
	_, err := postJSON(ctx, c, baseURL, RenewPath(leaseID), tok, struct{}{}, &r)
	return r, err
}
