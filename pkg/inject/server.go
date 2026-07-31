// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package inject implements the HTTP endpoint that receives edge-trigger
// payloads (from k8s-event-watcher and any other source speaking the
// envelope.InjectPayload shape) and dispatches them into the mast
// runtime.
//
// For the v0.1 spike this endpoint is single-session and single-bearer.
// Multi-session substrate and the X-Asserted-Caller proxy-identity
// mechanism from core-agent's recipe are deferred.
package inject

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-steer/mast/pkg/envelope"
)

// ErrUnavailable, returned (or wrapped) by a Handler or ResumeHandler,
// tells the server the daemon is refusing new work — a shutdown drain
// is underway. The server maps it to 503 + Retry-After instead of the
// generic 500, so emitters retry against the replacement pod rather
// than treating a rolling restart as a crash.
var ErrUnavailable = errors.New("daemon is shutting down; not accepting new work")

// ErrBadPayload, returned (or wrapped) by a Handler, marks a request
// the daemon refuses on its content (e.g. a payload UID deriving a
// reserved session ID). Mapped to 400 instead of the generic 500 so
// emitters don't retry a request that can never succeed.
var ErrBadPayload = errors.New("invalid inject payload")

// Handler receives a validated inject payload and drives the mast
// runtime. It returns an error if dispatch fails; the server maps that
// to a 5xx response (503 + Retry-After for ErrUnavailable).
type Handler func(ctx context.Context, payload envelope.InjectPayload) error

// ResumeRequest is the operator's answer to a pending HITL interrupt.
type ResumeRequest struct {
	// SessionID identifies the paused session (e.g. "incident-<uid>").
	SessionID string `json:"session_id"`

	// InterruptID matches the pending RequestInput's InterruptID.
	InterruptID string `json:"interrupt_id"`

	// Response is the reply payload; validated against the interrupt's
	// ResponseSchema by the workflow engine on resume.
	Response any `json:"response"`
}

// ResumeHandler feeds a resume payload into the runtime. Optional; when
// nil the /resume route responds 404.
type ResumeHandler func(ctx context.Context, req ResumeRequest) error

// AbortRequest asks the daemon to mark a session aborted.
//
// Semantics are those of pkg/transcript's Store.Abort — a durable
// operator-abort marker appended to the session's event log, not
// preemption of in-flight work. See that method's doc for the full
// contract (docs/durable-execution-design.md, "Operator-facing
// surface"; engine-level terminal abort is v0.2).
type AbortRequest struct {
	// SessionID identifies the session to mark aborted.
	SessionID string `json:"session_id"`

	// Reason is the operator-supplied reason, recorded in the abort
	// marker and surfaced by `mast sessions list/show`.
	Reason string `json:"reason,omitempty"`
}

// AbortHandler applies an abort request. Optional; when nil the /abort
// route responds 404.
type AbortHandler func(ctx context.Context, req AbortRequest) error

// Config configures the inject server.
type Config struct {
	// Listen is the bind address, e.g. ":7777".
	Listen string

	// BearerToken is the shared secret required in the Authorization
	// header. Empty disables auth (intended only for local development;
	// production deploys must set it).
	BearerToken string

	// Handler is called for each valid inject. Required.
	Handler Handler

	// ResumeHandler is called for each valid resume POST. Optional.
	ResumeHandler ResumeHandler

	// AbortHandler is called for each valid abort POST. Optional.
	AbortHandler AbortHandler

	// Logger is the structured logger. Defaults to slog.Default().
	Logger *slog.Logger

	// Metrics, when non-nil, is served at GET /metrics (Prometheus
	// scrape). Unauthenticated by design — scrape configs don't carry
	// the inject bearer token, and the payload is aggregate counters
	// only. Nil leaves the route unregistered.
	Metrics http.Handler

	// BaseContext, when non-nil, is the context every request context
	// derives from. The daemon passes its turn-lifetime context so
	// that when the shutdown drain window elapses, in-flight handler
	// turns are cancelled and unwind instead of dying at process exit.
	BaseContext context.Context
}

// Server is the HTTP inject endpoint.
type Server struct {
	cfg    Config
	logger *slog.Logger
	srv    *http.Server
}

// New constructs a Server. It does not start listening; call ListenAndServe.
func New(cfg Config) (*Server, error) {
	if cfg.Handler == nil {
		return nil, errors.New("inject: Handler is required")
	}
	if cfg.Listen == "" {
		cfg.Listen = ":7777"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{cfg: cfg, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleHealth)
	mux.HandleFunc("POST /inject", s.handleInject)
	mux.HandleFunc("POST /resume", s.handleResume)
	mux.HandleFunc("POST /abort", s.handleAbort)
	if cfg.Metrics != nil {
		mux.Handle("GET /metrics", cfg.Metrics)
	}
	s.srv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if cfg.BaseContext != nil {
		s.srv.BaseContext = func(net.Listener) context.Context { return cfg.BaseContext }
	}
	return s, nil
}

// ListenAndServe blocks serving requests. Returns http.ErrServerClosed
// on graceful shutdown.
func (s *Server) ListenAndServe() error {
	s.logger.Info("inject server listening", "addr", s.cfg.Listen, "auth_required", s.cfg.BearerToken != "")
	return s.srv.ListenAndServe()
}

// Shutdown attempts a graceful stop.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	defer func() { _ = r.Body.Close() }()

	var payload envelope.InjectPayload
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		s.logger.Warn("inject decode failed", "error", err.Error())
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.logger.Info("inject received",
		"kind", payload.Kind,
		"reason", payload.Reason,
		"namespace", payload.Namespace,
		"name", payload.Name,
		"uid", payload.UID,
		"cluster", payload.Cluster,
	)

	if err := s.cfg.Handler(r.Context(), payload); err != nil {
		if errors.Is(err, ErrUnavailable) {
			// Generic body on purpose: the 500 path below hides error
			// detail, and this path must not become a leak if a later
			// caller wraps context into the drain error.
			w.Header().Set("Retry-After", "10")
			http.Error(w, "shutting down; retry against the replacement instance", http.StatusServiceUnavailable)
			return
		}
		if errors.Is(err, ErrBadPayload) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Error("inject dispatch failed", "error", err.Error())
		http.Error(w, "dispatch failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintln(w, "accepted")
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.cfg.ResumeHandler == nil {
		http.Error(w, "resume not enabled", http.StatusNotFound)
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req ResumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" || req.InterruptID == "" {
		http.Error(w, "bad request: session_id and interrupt_id are required", http.StatusBadRequest)
		return
	}

	s.logger.Info("resume received", "session", req.SessionID, "interrupt_id", req.InterruptID)

	if err := s.cfg.ResumeHandler(r.Context(), req); err != nil {
		if errors.Is(err, ErrUnavailable) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "shutting down; retry against the replacement instance", http.StatusServiceUnavailable)
			return
		}
		if errors.Is(err, ErrBadPayload) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Error("resume dispatch failed", "error", err.Error())
		http.Error(w, "resume failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintln(w, "resumed")
}

func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.cfg.AbortHandler == nil {
		http.Error(w, "abort not enabled", http.StatusNotFound)
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req AbortRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		http.Error(w, "bad request: session_id is required", http.StatusBadRequest)
		return
	}

	s.logger.Info("abort received", "session", req.SessionID, "reason", req.Reason)

	if err := s.cfg.AbortHandler(r.Context(), req); err != nil {
		if errors.Is(err, ErrBadPayload) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Error("abort failed", "error", err.Error())
		http.Error(w, "abort failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintln(w, "aborted")
}

func (s *Server) authOK(r *http.Request) bool {
	if s.cfg.BearerToken == "" {
		return true // auth disabled
	}
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	presented := h[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.BearerToken)) == 1
}
