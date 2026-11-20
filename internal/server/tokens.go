package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/majorcontext/ward/internal/identity/usertoken"
)

const (
	maxTokenBody   = 4 << 10
	maxTokenRepos  = 64
	maxPrincipal   = 256
	maxTokenTTLSec = 10 * 365 * 24 * 3600
)

type createTokenRequest struct {
	Principal  string   `json:"principal"`
	Scope      string   `json:"scope"`
	Repos      []string `json:"repos"`
	TTLSeconds int64    `json:"ttl_seconds"`
}

type tokenView struct {
	ID        string   `json:"id"`
	Principal string   `json:"principal"`
	Scope     string   `json:"scope"`
	Repos     []string `json:"repos"`
	CreatedAt string   `json:"created_at"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	RevokedAt string   `json:"revoked_at,omitempty"`
}

type createTokenResponse struct {
	Token     string    `json:"token"`
	UserToken tokenView `json:"user_token"`
}

func viewOfToken(r usertoken.Record) tokenView {
	at := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	return tokenView{ID: r.ID, Principal: r.Principal, Scope: r.Scope, Repos: append([]string{}, r.Repos...),
		CreatedAt: at(r.CreatedAt), ExpiresAt: at(r.ExpiresAt), RevokedAt: at(r.RevokedAt)}
}

// tokenAdmin returns true for an operator token. A human identity never reaches it. Else it has written the answer.
func (s *Server) tokenAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.d.UserTokens == nil {
		writeError(w, codeNotFound, "not found")
		return false
	}
	return s.operator(w, r)
}

func (s *Server) createUserToken(w http.ResponseWriter, r *http.Request) {
	if !s.tokenAdmin(w, r) {
		return
	}
	var req createTokenRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxTokenBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Principal == "" || len(req.Principal) > maxPrincipal || strings.ContainsAny(req.Principal, " \t\r\n") ||
		len(req.Repos) > maxTokenRepos || req.TTLSeconds < 0 || req.TTLSeconds > maxTokenTTLSec || !usertoken.Valid(req.Scope, req.Repos) || slices.Contains(req.Repos, "") {
		writeError(w, codeInvalidBody, "the body needs principal, scope land and repos")
		return
	}
	tok, id, hash, err := usertoken.New()
	rec := usertoken.Record{ID: id, Principal: req.Principal, Scope: req.Scope, Repos: req.Repos, CreatedAt: s.d.Now()}
	if req.TTLSeconds > 0 {
		rec.ExpiresAt = rec.CreatedAt.Add(time.Duration(req.TTLSeconds) * time.Second)
	}
	if err == nil {
		err = s.d.UserTokens.CreateUserToken(r.Context(), hash, rec)
	}
	if err != nil {
		s.d.Logger.Error("create user token", "error", err.Error())
		writeError(w, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, createTokenResponse{Token: tok, UserToken: viewOfToken(rec)})
}

func (s *Server) listUserTokens(w http.ResponseWriter, r *http.Request) {
	if !s.tokenAdmin(w, r) {
		return
	}
	rows, err := s.d.UserTokens.UserTokens(r.Context())
	if err != nil {
		s.d.Logger.Error("list user tokens", "error", err.Error())
		writeError(w, codeInternal, "internal error")
		return
	}
	out := struct {
		Tokens []tokenView `json:"tokens"`
	}{Tokens: make([]tokenView, len(rows))}
	for i, row := range rows {
		out.Tokens[i] = viewOfToken(row)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) revokeUserToken(w http.ResponseWriter, r *http.Request) {
	if !s.tokenAdmin(w, r) {
		return
	}
	switch err := s.d.UserTokens.RevokeUserToken(r.Context(), r.PathValue("id"), s.d.Now()); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, usertoken.ErrNotFound):
		writeError(w, codeNotFound, "not found")
	default:
		s.d.Logger.Error("revoke user token", "error", err.Error())
		writeError(w, codeInternal, "internal error")
	}
}
