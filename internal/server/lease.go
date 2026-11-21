package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/majorcontext/ward/internal/dispatch/pull"
	"github.com/majorcontext/ward/internal/token"
	"github.com/majorcontext/ward/runnerproto"
)

const maxLeaseBody = 4 << 10

// runner returns the claims of a runner token, or writes the answer and returns false.
func (s *Server) runner(w http.ResponseWriter, r *http.Request) (token.Claims, bool) {
	c, ok := s.authenticate(w, r)
	if !ok {
		return c, false
	}
	if c.Scope != token.ScopeRunner {
		writeError(w, codeForbidden, "forbidden")
		return c, false
	}
	if s.d.Pull == nil {
		writeError(w, codeNotFound, "not found")
		return c, false
	}
	return c, true
}

func (s *Server) lease(w http.ResponseWriter, r *http.Request) {
	c, ok := s.runner(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLeaseBody))
	var in runnerproto.LeaseRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err != nil || dec.Decode(&in) != nil || dec.More() {
		writeError(w, codeInvalidBody, "the body needs isolation")
		return
	}
	l, found, err := s.d.Pull.Lease(r.Context(), c.Kid, in.Isolation, runnerproto.LeaseWait)
	switch {
	case errors.Is(err, pull.ErrUnknownRunner), errors.Is(err, pull.ErrWeakIsolation):
		writeError(w, codeForbidden, "forbidden")
	case r.Context().Err() != nil:
	case err != nil:
		s.d.Logger.Error("lease", "error", err.Error())
		writeError(w, codeInternal, "internal error")
	case !found:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, l)
	}
}

func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	c, ok := s.runner(w, r)
	if !ok {
		return
	}
	switch out, err := s.d.Pull.Renew(r.Context(), c.Kid, r.PathValue("id")); {
	case err == nil:
		writeJSON(w, http.StatusOK, out)
	case errors.Is(err, pull.ErrLeaseLost):
		writeError(w, codeLeaseLost, "the lease ended")
	case errors.Is(err, pull.ErrUnknownRunner):
		writeError(w, codeForbidden, "forbidden")
	default:
		s.d.Logger.Error("renew lease", "error", err.Error())
		writeError(w, codeInternal, "internal error")
	}
}
