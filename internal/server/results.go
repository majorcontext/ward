package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/token"
)

const maxResultBody = 64 << 10

func (s *Server) postResult(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if c.Scope != token.ScopeCheck {
		writeError(w, codeForbidden, "forbidden")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResultBody))
	var p check.Posted
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err != nil || dec.Decode(&p) != nil || dec.More() || p.RequestID == "" {
		writeError(w, codeInvalidBody, "the body needs request_id, status, summary and log_url")
		return
	}
	switch err := s.d.Checks.Post(r.Context(), c.Repo, p); {
	case err == nil:
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, check.ErrInvalid):
		writeError(w, codeInvalidBody, "status is pass or fail, summary has at most 4096 bytes and log_url at most 2048")
	case errors.Is(err, check.ErrUnknownRequest):
		writeError(w, codeNotFound, "not found")
	case errors.Is(err, check.ErrOtherRepo):
		writeError(w, codeForbidden, "forbidden")
	default:
		s.d.Logger.Error("record result", "error", err.Error())
		writeError(w, codeInternal, "internal error")
	}
}
