package check

import (
	"context"
	"time"
)

// Status is the outcome that a runner reports.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
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
	// OpenRequest returns the open request for the key of r, or stores r and returns it with true.
	OpenRequest(ctx context.Context, r Request) (Request, bool, error)
	Request(ctx context.Context, id string) (Request, bool, error)
	OpenRequests(ctx context.Context) ([]Request, error)
	SetAccepted(ctx context.Context, id string, at time.Time) error
	// Supersede closes the request old and stores next, an open request for the same key.
	Supersede(ctx context.Context, old string, next Request) error
	// AddResult stores a result and closes its request. A pass also closes every other open request for the key.
	// It returns false and changes nothing when the request already has a result.
	AddResult(ctx context.Context, r Result) (bool, error)
	// Results returns the results of one key, oldest first.
	Results(ctx context.Context, repo, tree, hash, env string) ([]Result, error)
	MaxResultID(ctx context.Context) (int64, error)
}
