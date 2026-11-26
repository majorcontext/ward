// Package wardapi holds the wire types of the ward HTTP API that a caller and the server share: the land request, the land
// view and the error body with its codes.
package wardapi

import "github.com/majorcontext/motte/api"

// Code is the code of an error. It is the same text in an error body and in the error of a failed land.
type Code string

const (
	CodeUnauthorized     Code = "unauthorized"
	CodeForbidden        Code = "forbidden"
	CodeNotFound         Code = "not_found"
	CodeInvalidBody      Code = "invalid_body"
	CodeMethodNotAllowed Code = "method_not_allowed"
	CodeInternal         Code = "internal_error"
	CodeLandConflict     Code = "land_conflict"
	CodeLandInvalid      Code = "land_invalid"
	CodeRefNotAllowed    Code = "ref_not_allowed"
	CodeLandIDInUse      Code = "land_id_in_use"
	CodeLandTimeout      Code = "land_timeout"
	CodeLeaseLost        Code = "lease_lost"
	CodeCheckFailed      Code = "check_failed"
	CodeLandStale        Code = "land_stale"
)

// Error is the error of a response and the error of a land that failed.
type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// ErrorBody is the body of every error response.
type ErrorBody struct {
	Error Error `json:"error"`
}

// LandState is the state of a land.
type LandState string

const (
	LandQueued   LandState = "queued"
	LandChecking LandState = "checking"
	LandLanding  LandState = "landing"
	LandLanded   LandState = "landed"
	LandFailed   LandState = "failed"
)

// Ended reports whether the land has ended.
func (s LandState) Ended() bool { return s == LandLanded || s == LandFailed }

// CheckStatus is the state of one check for one tree.
type CheckStatus string

const (
	CheckPending CheckStatus = "pending"
	CheckPass    CheckStatus = "pass"
	CheckFail    CheckStatus = "fail"
	CheckFlaky   CheckStatus = "flaky"
)

// LandCheck is one required check of a land and its status.
type LandCheck struct {
	Name  string      `json:"name"`
	State CheckStatus `json:"state"`
}

// LandRequest is the body of POST /v1/lands.
type LandRequest struct {
	ID     string `json:"id,omitempty"`
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	Branch string `json:"branch"`
	// Base is the commit that the branch was built on: the merge base of the branch and the ref, which the caller finds
	// with git from its own history.
	Base         string `json:"base,omitempty"`
	DeleteBranch bool   `json:"delete_branch"`
}

// Land is the view of a land that the API answers.
type Land struct {
	ID            string             `json:"id"`
	Repo          string             `json:"repo"`
	Ref           string             `json:"ref"`
	Branch        string             `json:"branch"`
	State         LandState          `json:"state"`
	Tree          string             `json:"tree"`
	Checks        []LandCheck        `json:"checks"`
	Tip           string             `json:"tip"`
	Generation    int64              `json:"generation"`
	FastForward   bool               `json:"fast_forward"`
	Commits       []api.LandedCommit `json:"commits"`
	BranchDeleted bool               `json:"branch_deleted"`
	Error         *Error             `json:"error"`
}
