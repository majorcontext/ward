package land

import (
	"context"

	"github.com/majorcontext/motte/api"

	"github.com/majorcontext/ward/internal/check"
)

// State is the state of a land.
type State string

const (
	Queued   State = "queued"
	Checking State = "checking"
	Landing  State = "landing"
	Landed   State = "landed"
	Failed   State = "failed"
)

// Ended reports whether the land has ended.
func (s State) Ended() bool { return s == Landed || s == Failed }

// The codes of a failed land.
const (
	CodeConflict    = "land_conflict"
	CodeInvalid     = "land_invalid"
	CodeTimeout     = "land_timeout"
	CodeCheckFailed = "check_failed"
	CodeStale       = "land_stale"
	CodeInternal    = "internal_error"
)

// CheckState is one required check and its state.
type CheckState struct {
	Name  string      `json:"name"`
	State check.State `json:"state"`
}

// Error says why a land failed.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// Call is the final land call to motte, stored before it is sent so that a repeat after a restart carries the same values.
type Call struct {
	Base      string `json:"base"`
	Head      string `json:"head"`
	Tree      string `json:"tree"`
	BranchGen int64  `json:"branch_generation"`
	// ID is the land id that motte sees when the call lands a batch. It is empty for a call of one land.
	ID string `json:"id,omitempty"`
	// Tip, FastForward and Commits are the part of a batch that belongs to this land.
	Tip         string             `json:"tip,omitempty"`
	FastForward bool               `json:"fast_forward,omitempty"`
	Commits     []api.LandedCommit `json:"commits,omitempty"`
}

// Land is the record of one land. Attempt counts the times that the land reached step 5, and Call is the call of the latest. Mark is the result position after which a fail counts for this land.
type Land struct {
	ID            string             `json:"id"`
	Repo          string             `json:"repo"`
	Ref           string             `json:"ref"`
	Branch        string             `json:"branch"`
	DeleteBranch  bool               `json:"delete_branch"`
	State         State              `json:"state"`
	Tree          string             `json:"tree"`
	Checks        []CheckState       `json:"checks"`
	Tip           string             `json:"tip"`
	Generation    int64              `json:"generation"`
	FastForward   bool               `json:"fast_forward"`
	Commits       []api.LandedCommit `json:"commits"`
	BranchDeleted bool               `json:"branch_deleted"`
	Error         *Error             `json:"error,omitempty"`
	Retries       int                `json:"retries"`
	Mark          int64              `json:"mark"`
	Attempt       int                `json:"attempt"`
	Call          *Call              `json:"call,omitempty"`
}

// Key names the queue of a land.
type Key struct{ Repo, Ref string }

// Store is the transactional state of the lands. Each method that writes is one transaction.
type Store interface {
	// InsertLand stores a new land. Lands of one key keep the order of insertion.
	InsertLand(ctx context.Context, l Land) error
	Land(ctx context.Context, id string) (Land, bool, error)
	// NextLands returns up to n of the oldest lands of the key that have not ended, oldest first.
	NextLands(ctx context.Context, key Key, n int) ([]Land, error)
	SaveLand(ctx context.Context, l Land) error
	// UnfinishedKeys returns the key of every land that has not ended.
	UnfinishedKeys(ctx context.Context) ([]Key, error)
}
