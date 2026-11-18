package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"

	"github.com/majorcontext/ward/internal/land"
	"github.com/majorcontext/ward/internal/token"
)

const maxLandBody = 64 << 10

type landRequest struct {
	Repo         string `json:"repo"`
	Ref          string `json:"ref"`
	Branch       string `json:"branch"`
	DeleteBranch bool   `json:"delete_branch"`
}

type landResponse struct {
	Ref           string             `json:"ref"`
	Generation    int64              `json:"generation"`
	Tip           string             `json:"tip"`
	FastForward   bool               `json:"fast_forward"`
	Commits       []api.LandedCommit `json:"commits"`
	BranchDeleted bool               `json:"branch_deleted"`
}

func (s *Server) land(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if c.Scope != token.ScopeLand {
		writeError(w, codeForbidden, "forbidden")
		return
	}
	if s.d.Land == nil {
		writeError(w, codeNotFound, "not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLandBody))
	var req landRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err != nil || dec.Decode(&req) != nil || req.Repo == "" || req.Branch == "" {
		writeError(w, codeInvalidBody, "the body needs repo and branch")
		return
	}
	if req.Repo != c.Repo {
		writeError(w, codeForbidden, "forbidden")
		return
	}
	if req.Ref == "" {
		req.Ref = "main"
	}
	res, err := s.d.Land.Land(r.Context(), land.Request{Repo: req.Repo, Ref: req.Ref, Branch: req.Branch, DeleteBranch: req.DeleteBranch})
	if err != nil {
		s.landError(w, err)
		return
	}
	commits := res.Commits
	if commits == nil {
		commits = []api.LandedCommit{}
	}
	writeJSON(w, http.StatusOK, landResponse{Ref: res.Ref, Generation: res.Generation, Tip: res.Tip, FastForward: res.FastForward,
		Commits: commits, BranchDeleted: res.BranchDeleted})
}

func (s *Server) landError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, client.ErrLandConflict):
		paths := client.ConflictPaths(err)
		if paths == nil {
			paths = []string{}
		}
		writeErrorDetails(w, codeLandConflict, "the branch changes paths that changed on the ref", map[string]any{"paths": paths})
	case errors.Is(err, client.ErrLandInvalid), errors.Is(err, land.ErrNoMergeBase):
		writeError(w, codeLandInvalid, "the branch cannot land on the ref")
	case errors.Is(err, client.ErrLandTimeout):
		writeError(w, codeLandTimeout, "the land did not finish in time")
	case errors.Is(err, client.ErrNotFound):
		writeError(w, codeNotFound, "the branch or the ref does not exist")
	default:
		s.d.Logger.Error("land", "error", err.Error())
		writeError(w, codeInternal, "internal error")
	}
}
