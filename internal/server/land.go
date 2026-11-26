package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/majorcontext/motte/api"

	"github.com/majorcontext/ward/internal/land"
	"github.com/majorcontext/ward/internal/token"
	"github.com/majorcontext/ward/wardapi"
)

const (
	maxLandBody = 64 << 10
	// bodyReadLimit bounds the read of a body before the caller is known.
	bodyReadLimit = 10 * time.Second
)

type landRequest = wardapi.LandRequest

func viewOf(l land.Land) wardapi.Land {
	v := wardapi.Land{ID: l.ID, Repo: l.Repo, Ref: l.Ref, Branch: l.Branch, State: l.State, Tree: l.Tree, Checks: l.Checks, Tip: l.Tip,
		Generation: l.Generation, FastForward: l.FastForward, Commits: l.Commits, BranchDeleted: l.BranchDeleted, Error: l.Error}
	if v.Checks == nil {
		v.Checks = []wardapi.LandCheck{}
	}
	if v.Commits == nil {
		v.Commits = []api.LandedCommit{}
	}
	return v
}

// landClaims returns the claims of a land token, or of a human identity for the repository that repo names, or
// writes the refusal and returns false. repo runs only when the human identity is tried.
func (s *Server) landClaims(w http.ResponseWriter, r *http.Request, repo func() string) (token.Claims, bool) {
	c, err := s.bearer.Authenticate(r, "")
	if err != nil && s.d.Human != nil {
		c, err = s.d.Human.Authenticate(r, repo())
	}
	switch {
	case err != nil:
		writeError(w, codeUnauthorized, "unauthorized")
	case c.Scope != token.ScopeLand:
		writeError(w, codeForbidden, "forbidden")
	default:
		return c, true
	}
	return c, false
}

func (s *Server) land(w http.ResponseWriter, r *http.Request) {
	var req landRequest
	var bodyErr error
	loaded := false
	load := func() {
		if loaded {
			return
		}
		loaded = true
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadLimit))
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLandBody))
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if bodyErr = dec.Decode(&req); err != nil {
			bodyErr = err
		}
	}
	c, ok := s.landClaims(w, r, func() string { load(); return req.Repo })
	if !ok {
		return
	}
	load()
	if bodyErr != nil || req.Repo == "" || req.Branch == "" {
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
	if !validLandID(req.ID) {
		writeError(w, codeInvalidBody, "id is 8 to 48 characters of a-z, A-Z, 0-9, _ and -, and begins with a letter or a digit")
		return
	}
	if req.ID != "" {
		if l, ok, err := s.d.Land.Repeat(r.Context(), land.Request{ID: req.ID, Repo: req.Repo, Ref: req.Ref, Branch: req.Branch, DeleteBranch: req.DeleteBranch}); err != nil {
			s.landEnqueueError(w, err)
			return
		} else if ok {
			writeJSON(w, http.StatusAccepted, viewOf(l))
			return
		}
	}
	if !s.d.Land.RefAllowed(req.Ref) {
		writeError(w, codeRefNotAllowed, "no pattern of the land configuration names the ref")
		return
	}
	l, err := s.d.Land.Enqueue(r.Context(), land.Request{ID: req.ID, Repo: req.Repo, Ref: req.Ref, Branch: req.Branch, DeleteBranch: req.DeleteBranch})
	if err != nil {
		s.landEnqueueError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, viewOf(l))
}

func (s *Server) getLand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	repo := func() string {
		l, ok, err := s.d.Land.Get(r.Context(), id, 0, "")
		if err != nil || !ok {
			return ""
		}
		return l.Repo
	}
	c, ok := s.landClaims(w, r, repo)
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

// validLandID accepts an empty id and an id of 8 to 48 characters that begins with a letter or a digit. The id becomes a
// land id of motte with a suffix, and part of a ref name, so it holds only what both accept.
func validLandID(id string) bool {
	if id == "" {
		return true
	}
	if len(id) < 8 || len(id) > 48 {
		return false
	}
	for i, c := range id {
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || c != '_' && c != '-') {
			return false
		}
	}
	return true
}

func (s *Server) landEnqueueError(w http.ResponseWriter, err error) {
	if errors.Is(err, land.ErrIDInUse) {
		writeError(w, codeLandIDInUse, "the id belongs to another land")
		return
	}
	s.d.Logger.Error("record land", "error", err.Error())
	writeError(w, codeInternal, "internal error")
}
