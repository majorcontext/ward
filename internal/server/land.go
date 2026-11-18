package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/majorcontext/motte/api"

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

type landView struct {
	ID            string             `json:"id"`
	Repo          string             `json:"repo"`
	Ref           string             `json:"ref"`
	Branch        string             `json:"branch"`
	State         land.State         `json:"state"`
	Tree          string             `json:"tree"`
	Checks        []land.CheckState  `json:"checks"`
	Tip           string             `json:"tip"`
	Generation    int64              `json:"generation"`
	FastForward   bool               `json:"fast_forward"`
	Commits       []api.LandedCommit `json:"commits"`
	BranchDeleted bool               `json:"branch_deleted"`
	Error         *land.Error        `json:"error"`
}

func viewOf(l land.Land) landView {
	v := landView{ID: l.ID, Repo: l.Repo, Ref: l.Ref, Branch: l.Branch, State: l.State, Tree: l.Tree, Checks: l.Checks, Tip: l.Tip,
		Generation: l.Generation, FastForward: l.FastForward, Commits: l.Commits, BranchDeleted: l.BranchDeleted, Error: l.Error}
	if v.Checks == nil {
		v.Checks = []land.CheckState{}
	}
	if v.Commits == nil {
		v.Commits = []api.LandedCommit{}
	}
	return v
}

// landClaims returns the claims of a land token, or writes the refusal and returns false.
func (s *Server) landClaims(w http.ResponseWriter, r *http.Request) (token.Claims, bool) {
	c, ok := s.authenticate(w, r)
	if !ok {
		return c, false
	}
	if c.Scope != token.ScopeLand {
		writeError(w, codeForbidden, "forbidden")
		return c, false
	}
	return c, true
}

func (s *Server) land(w http.ResponseWriter, r *http.Request) {
	c, ok := s.landClaims(w, r)
	if !ok {
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
	l, err := s.d.Land.Enqueue(r.Context(), land.Request{Repo: req.Repo, Ref: req.Ref, Branch: req.Branch, DeleteBranch: req.DeleteBranch})
	if err != nil {
		s.d.Logger.Error("record land", "error", err.Error())
		writeError(w, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusAccepted, viewOf(l))
}

func (s *Server) getLand(w http.ResponseWriter, r *http.Request) {
	c, ok := s.landClaims(w, r)
	if !ok {
		return
	}
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > int(land.MaxWait/time.Second) {
			writeError(w, codeInvalidBody, "wait must be 0 to 60 seconds")
			return
		}
		wait = time.Duration(n) * time.Second
	}
	known := land.State(r.URL.Query().Get("state"))
	switch known {
	case "", land.Queued, land.Checking, land.Landing, land.Landed, land.Failed:
	default:
		writeError(w, codeInvalidBody, "state is queued, checking, landing, landed or failed")
		return
	}
	id := r.PathValue("id")
	l, ok, err := s.d.Land.Get(r.Context(), id, 0, "")
	if err == nil && ok && l.Repo == c.Repo && wait > 0 {
		l, ok, err = s.d.Land.Get(r.Context(), id, wait, known)
	}
	switch {
	case r.Context().Err() != nil:
	case err != nil:
		s.d.Logger.Error("read land", "error", err.Error())
		writeError(w, codeInternal, "internal error")
	case !ok:
		writeError(w, codeNotFound, "not found")
	case l.Repo != c.Repo:
		writeError(w, codeForbidden, "forbidden")
	default:
		writeJSON(w, http.StatusOK, viewOf(l))
	}
}
