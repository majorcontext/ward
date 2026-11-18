package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/majorcontext/ward/internal/mirror"
	"github.com/majorcontext/ward/internal/token"
)

const maxRefs = 1000

type refStatus struct {
	Ref          string `json:"ref"`
	State        string `json:"state"`
	Pushed       string `json:"pushed"`
	PushedAt     string `json:"pushed_at"`
	Failure      string `json:"failure"`
	Message      string `json:"message"`
	Attempts     int    `json:"attempts"`
	FailureSince string `json:"failure_since"`
}

type mirrorStatus struct {
	Name   string      `json:"name"`
	Repo   string      `json:"repo"`
	State  string      `json:"state"`
	Refs   []refStatus `json:"refs,omitempty"`
	Cursor string      `json:"cursor,omitempty"`
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func refState(r mirror.Ref) string {
	switch {
	case r.Failure != "":
		return "failing"
	case r.Dirty:
		return "pending"
	}
	return "synced"
}

func mirrorState(s mirror.Stats) string {
	switch {
	case len(s.Failing) > 0:
		return "failing"
	case s.Dirty > 0 || !s.Reconciled:
		return "pending"
	}
	return "synced"
}

func (s *Server) summary(ctx context.Context, en mirror.Entry) (mirrorStatus, error) {
	st := mirrorStatus{Name: en.Name, Repo: en.Repo, State: "pending"}
	stats, err := s.d.Engine.Stats(ctx)
	if err != nil {
		return st, err
	}
	for _, x := range stats {
		if x.Mirror == en.Name {
			st.State = mirrorState(x)
		}
	}
	if seed := s.d.Engine.SeedState(en.Name); seed != "" {
		st.State = seed
	}
	return st, nil
}

func (s *Server) listMirrors(w http.ResponseWriter, r *http.Request) {
	if !s.operator(w, r) {
		return
	}
	out := []mirrorStatus{}
	if s.d.Engine != nil {
		for _, en := range s.d.Engine.Mirrors() {
			st, err := s.summary(r.Context(), en)
			if err != nil {
				writeError(w, codeInternal, "internal error")
				return
			}
			out = append(out, st)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mirrors": out})
}

func (s *Server) getMirror(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var en mirror.Entry
	found := false
	if s.d.Engine != nil {
		en, found = s.d.Engine.Mirror(r.PathValue("name"))
	}
	switch {
	case c.Scope != token.ScopeRead && c.Scope != token.ScopeOperator:
		writeError(w, codeForbidden, "forbidden")
		return
	case c.Scope == token.ScopeRead && (!found || c.Repo != en.Repo):
		writeError(w, codeForbidden, "forbidden")
		return
	case !found:
		writeError(w, codeNotFound, "not found")
		return
	}
	after := ""
	if cur := r.URL.Query().Get("cursor"); cur != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cur)
		if err != nil || len(raw) == 0 {
			writeError(w, codeInvalidBody, "the cursor is not valid")
			return
		}
		after = string(raw)
	}
	st, err := s.summary(r.Context(), en)
	var refs []mirror.Ref
	if err == nil {
		refs, err = s.d.Engine.Refs(r.Context(), en.Name, after, maxRefs+1)
	}
	if err != nil {
		writeError(w, codeInternal, "internal error")
		return
	}
	if len(refs) > maxRefs {
		refs = refs[:maxRefs]
		st.Cursor = base64.RawURLEncoding.EncodeToString([]byte(refs[len(refs)-1].Name))
	}
	st.Refs = make([]refStatus, 0, len(refs))
	for _, ref := range refs {
		st.Refs = append(st.Refs, refStatus{Ref: ref.Name, State: refState(ref), Pushed: ref.Pushed, PushedAt: stamp(ref.PushedAt),
			Failure: string(ref.Failure), Message: ref.Message, Attempts: ref.Attempts, FailureSince: stamp(ref.FailureSince)})
	}
	writeJSON(w, http.StatusOK, st)
}
