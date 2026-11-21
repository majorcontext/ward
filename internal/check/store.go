package check

import (
	"context"
	"time"

	"github.com/majorcontext/ward/runnerproto"
)

// Status is the outcome that a runner reports.
type Status = runnerproto.Status

const (
	StatusPass = runnerproto.Pass
	StatusFail = runnerproto.Fail
)

// Request is one check request to a runner. It is open until a result or a newer request for its key ends it.
type Request struct {
	ID         string
	Repo       string
	Tree       string
	Commit     string
	Check      Check
	Hash       string
	Env        string
	AcceptedAt time.Time
	Open       bool
}

// Result is a fact about the content of a tree. Its ID orders results across the store.
type Result struct {
	ID        int64
	RequestID string
	Repo      string
	Tree      string
	Hash      string
	Env       string
	Status    Status
	Summary   string
	LogURL    string
}

// Store is the transactional state of requests and results. Each method that writes is one transaction.
type Store interface {
	// OpenRequest returns the open request for the key of r. With no open request and no pass for the key, it stores r and returns it with true.
	OpenRequest(ctx context.Context, r Request) (Request, bool, error)
	Request(ctx context.Context, id string) (Request, bool, error)
	OpenRequests(ctx context.Context) ([]Request, error)
	SetAccepted(ctx context.Context, id string, at time.Time) error
	// Supersede closes the request old and stores next, an open request for the same key. It does nothing and returns false
	// when old is no longer open or the key has a pass.
	Supersede(ctx context.Context, old string, next Request) (bool, error)
	// AddResult stores a result and closes its request. A pass also closes every other open request for the key.
	// It returns false and changes nothing when the request already has a result.
	AddResult(ctx context.Context, r Result) (bool, error)
	// Results returns the results of one key, oldest first.
	Results(ctx context.Context, repo, tree, hash, env string) ([]Result, error)
	MaxResultID(ctx context.Context) (int64, error)
	// Lease gives kid, under leaseID, the oldest open request of one of repos that has no live lease, until the time until.
	// A lease that passed its time is dead, so its request is leasable again with the same ID.
	Lease(ctx context.Context, repos []string, kid, leaseID string, now, until time.Time) (Request, bool, error)
	// RenewLease moves the end of a lease that kid holds. It returns false when the request is closed, when its repository
	// is not in repos, when a newer lease replaced the lease, or when kid does not hold it.
	RenewLease(ctx context.Context, leaseID, kid string, repos []string, until time.Time) (bool, error)
	// Leased reports whether kid has held a lease on the request.
	Leased(ctx context.Context, requestID, kid string) (bool, error)
}
