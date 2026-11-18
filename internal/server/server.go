// Package server is the HTTP surface of ward.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/majorcontext/motte/api"

	"github.com/majorcontext/ward/internal/mirror"
	"github.com/majorcontext/ward/internal/token"
)

const maxHookBody = 64 << 10

// Deps are what the handler needs. Engine is nil when no mirror is configured.
type Deps struct {
	Engine   *mirror.Engine
	Store    mirror.Store
	Verifier *token.Verifier
	Registry *prometheus.Registry
	Logger   *slog.Logger
	Now      func() time.Time
}

// Server is the handler of every route.
type Server struct {
	d        Deps
	mux      *http.ServeMux
	requests *prometheus.CounterVec
}

// New returns the handler.
func New(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Registry == nil {
		d.Registry = prometheus.NewRegistry()
	}
	s := &Server{d: d, mux: http.NewServeMux()}
	s.requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ward_http_requests_total", Help: "Requests by route."}, []string{"route"})
	d.Registry.MustRegister(s.requests)
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.mux.HandleFunc("GET /ready", s.ready)
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(d.Registry, promhttp.HandlerOpts{}))
	s.mux.HandleFunc("POST "+hookRoute, s.hook)
	s.mux.HandleFunc("POST "+hookRoute+"/reset", s.reset)
	s.mux.HandleFunc("GET /v1/mirrors", s.listMirrors)
	s.mux.HandleFunc("GET /v1/mirrors/{name}", s.getMirror)
	s.mux.HandleFunc("POST /v1/mirrors/{name}/reconcile", s.reconcile)
	return s
}

const hookRoute = "/v1/hooks/motte"

// statusRecorder notes whether a handler sent a status.
type statusRecorder struct {
	http.ResponseWriter
	wrote bool
}

func (r *statusRecorder) WriteHeader(status int) {
	r.wrote = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// probe records the status that a mux error handler picks and drops its body.
type probe struct {
	header http.Header
	status int
}

func (p *probe) Header() http.Header         { return p.header }
func (p *probe) WriteHeader(status int)      { p.status = status }
func (p *probe) Write(b []byte) (int, error) { return len(b), nil }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	route := "unmatched"
	defer func() {
		if p := recover(); p != nil {
			s.d.Logger.Error("handler panic", "route", route, "panic", p, "stack", string(debug.Stack()))
			if !rec.wrote {
				writeError(rec, codeInternal, "internal error")
			}
		}
		s.requests.WithLabelValues(route).Inc()
	}()
	h, pattern := s.mux.Handler(r)
	if pattern != "" {
		route = pattern
		s.mux.ServeHTTP(rec, r)
		return
	}
	pr := &probe{header: w.Header()}
	h.ServeHTTP(pr, r)
	if pr.status == http.StatusMethodNotAllowed {
		writeError(rec, codeMethodNotAllowed, "method not allowed")
		return
	}
	writeError(rec, codeNotFound, "not found")
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.d.Store == nil || s.d.Store.Ping(ctx) != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	if s.d.Engine == nil {
		writeError(w, codeNotFound, "not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		writeError(w, codeInvalidBody, "the body is too large or unreadable")
		return
	}
	switch err := s.d.Engine.Deliver(r.Context(), body, r.Header.Get(api.HeaderSignature)); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, mirror.ErrSignature):
		writeError(w, codeUnauthorized, "unauthorized")
	case errors.Is(err, mirror.ErrBody):
		writeError(w, codeInvalidBody, "the body is not a ref event")
	default:
		s.d.Logger.Error("record delivery", "error", err.Error())
		writeError(w, codeInternal, "internal error")
	}
}

// authenticate returns the claims of the bearer token, or writes 401 and returns false.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (token.Claims, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if ok && s.d.Verifier != nil {
		if c, err := s.d.Verifier.Verify(tok, s.d.Now()); err == nil {
			return c, true
		}
	}
	writeError(w, codeUnauthorized, "unauthorized")
	return token.Claims{}, false
}

// operator returns true when the caller holds an operator token. Else it has written the answer.
func (s *Server) operator(w http.ResponseWriter, r *http.Request) bool {
	c, ok := s.authenticate(w, r)
	if ok && c.Scope != token.ScopeOperator {
		writeError(w, codeForbidden, "forbidden")
		return false
	}
	return ok
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	if !s.operator(w, r) {
		return
	}
	if s.d.Engine == nil {
		writeError(w, codeNotFound, "not found")
		return
	}
	s.d.Engine.RequestReset()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	if !s.operator(w, r) {
		return
	}
	if s.d.Engine == nil || !s.d.Engine.RequestReconcile(r.PathValue("name")) {
		writeError(w, codeNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
