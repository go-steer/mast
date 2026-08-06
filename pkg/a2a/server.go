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

// This file implements mast as an A2A *server* (docs/a2a-design.md,
// "Mast as A2A server"): agent-card publication, the single JSON-RPC
// endpoint, and pluggable bearer auth with per-skill scope checks.
//
// Build-vs-buy (issue #78): hand-rolled over the wire types this package
// already owns rather than adopting ADK's adka2a server, because that
// server's Executor drives the runner directly and BYPASSES mast's turn
// chokepoint (cmd/mast runTurnPre) — where the entire durable-execution
// spine (budget, pause, abort, turn-lock, effects outbox) lives. The
// Backend seam below mirrors inject.Handler: this package never imports
// the runtime; the daemon wires task execution through runTurnPre.
//
// Stage A (this change) serves the card, tasks/get, and tasks/cancel and
// enforces auth; message/send and message/stream answer with the A2A
// "unsupported operation" error until Stages B/C land.

package a2a

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
)

// Principal is the authenticated caller a TokenValidator resolves a
// bearer token to. Scopes gate per-skill access (docs/a2a-design.md
// "Auth model"); Tenant, when set, propagates to WithIsolationScope on
// the session (Stage B).
type Principal struct {
	Subject string
	Scopes  []string
	Tenant  string
}

// hasScope reports whether the principal carries want.
func (p *Principal) hasScope(want string) bool {
	if p == nil {
		return false
	}
	for _, s := range p.Scopes {
		if s == want {
			return true
		}
	}
	return false
}

// TokenValidator resolves a bearer token to a Principal. It returns
// ErrInvalidToken for a token it does not recognize (→ HTTP 401); any
// other error is treated as a validator fault (→ HTTP 500). Built-in:
// StaticBearerValidator. JWT/JWKS, Google IAM, and OAuth2 introspection
// validators are v0.3 (docs/a2a-design.md "Auth model") — the interface
// ships now as the extension point.
type TokenValidator interface {
	Validate(ctx context.Context, token string) (*Principal, error)
}

// ErrInvalidToken marks an unrecognized bearer token; the server maps it
// to HTTP 401.
var ErrInvalidToken = errors.New("a2a: invalid or unknown bearer token")

// ErrTaskNotFound marks an unknown task id; the server maps it to the
// A2A TaskNotFound JSON-RPC error (-32001).
var ErrTaskNotFound = errors.New("a2a: task not found")

// StaticBearerValidator validates against a fixed token→Principal map —
// the "static bearer tokens (for simple deployments)" validator from
// docs/a2a-design.md. It compares in constant time across every
// configured token so neither a miss nor a value mismatch leaks timing.
type StaticBearerValidator struct {
	principals map[string]*Principal
}

// NewStaticBearerValidator builds a validator from a token→Principal
// map. At least one non-empty token with a non-nil principal is
// required.
func NewStaticBearerValidator(tokens map[string]*Principal) (*StaticBearerValidator, error) {
	if len(tokens) == 0 {
		return nil, errors.New("a2a: static bearer validator needs at least one token")
	}
	m := make(map[string]*Principal, len(tokens))
	for tok, p := range tokens {
		if tok == "" {
			return nil, errors.New("a2a: static bearer validator: empty token")
		}
		if p == nil {
			return nil, errors.New("a2a: static bearer validator: nil principal")
		}
		m[tok] = p
	}
	return &StaticBearerValidator{principals: m}, nil
}

// Validate resolves token to its principal, or ErrInvalidToken. It
// iterates every entry with a constant-time compare so total time does
// not depend on which (if any) token matched.
func (v *StaticBearerValidator) Validate(_ context.Context, token string) (*Principal, error) {
	var match *Principal
	for tok, p := range v.principals {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1 {
			match = p
		}
	}
	if match == nil {
		return nil, ErrInvalidToken
	}
	return match, nil
}

// ExposedSkill is one workload's A2A exposure, projected by the daemon
// from the bundle's a2a: section (this package does not import
// pkg/workload). One skill per exposed workload.
type ExposedSkill struct {
	// WorkloadName is the mast workload backing this skill; the task
	// registry maps a submitted skill call to it (Stage B), and task
	// verbs resolve required scopes through it.
	WorkloadName string

	// SkillName is the A2A skill id/name (bundle a2a.skill_name).
	SkillName string

	// Description is rendered into the card skill. The daemon may fold
	// the mast-side input/output schema hints into it — spec AgentSkill
	// has no schema fields (docs/a2a-design.md note).
	Description string

	// Tags surface on the card skill; defaults to ["mast"] when empty.
	Tags []string

	// Scopes are required to invoke this skill. Empty means the skill
	// needs authentication only (a valid token, no specific scope) when
	// a validator is configured, or open access when it is not.
	Scopes []string
}

// TaskInfo is the backend's snapshot of a task (== a mast session).
type TaskInfo struct {
	// WorkloadName owns the task; it resolves the skill for scope checks.
	WorkloadName string

	// State is the A2A lifecycle state, mapped from the session's
	// log-proven state by the backend. A transcript-only read never
	// reports "completed" (the event log cannot prove a turn finished
	// versus is in flight) — that state comes from the in-process task
	// registry in Stage B.
	State TaskState

	// ContextID groups related messages; optional in Stage A.
	ContextID string

	// StatusMessage is an optional human-readable status line surfaced in
	// the task's status.message.
	StatusMessage string
}

// Backend drives task verbs against the mast runtime. The daemon
// implements it over the transcript store (GetTask), the abort machinery
// (CancelTask), and — in Stage B — runTurnPre (submit). This package
// never imports the runtime; the seam mirrors inject.Handler.
type Backend interface {
	// GetTask returns the task snapshot, or ErrTaskNotFound.
	GetTask(ctx context.Context, taskID string) (TaskInfo, error)

	// CancelTask requests cancellation (idempotent), returning the
	// resulting snapshot, or ErrTaskNotFound.
	CancelTask(ctx context.Context, taskID, reason string) (TaskInfo, error)
}

// TaskMetric records A2A task lifecycle outcomes. The daemon backs it
// with observability.Registry.A2ATask; nil disables. The outcome string
// is a TaskState value (fixed vocabulary — see observability.Prime).
type TaskMetric interface {
	A2ATask(workload, outcome string)
}

// Config configures the A2A server.
type Config struct {
	// Listen is the bind address, e.g. ":7780". Used by ListenAndServe.
	Listen string

	// Skills are the exposed workloads. An empty slice serves a card
	// with no skills; the daemon only starts the server when at least
	// one workload opts in.
	Skills []ExposedSkill

	// Validator authenticates every /a2a request. Nil disables auth
	// (dev only) — like inject's empty BearerToken. When set, a request
	// without a valid bearer is refused 401 before any dispatch.
	Validator TokenValidator

	// Backend is required.
	Backend Backend

	// CardName / CardDescription / CardVersion populate the aggregated
	// agent card. CardName defaults to "mast".
	CardName        string
	CardDescription string
	CardVersion     string

	// ExternalURL overrides the card's request-derived url.
	ExternalURL string

	// Metric, when non-nil, records task outcomes.
	Metric TaskMetric

	// Logger defaults to slog.Default().
	Logger *slog.Logger

	// BaseContext, when non-nil, is the context every request derives
	// from (the daemon passes its turn lifetime, as for inject).
	BaseContext context.Context
}

// Server is the A2A HTTP server. Construct with New; serve with
// ListenAndServe.
type Server struct {
	cfg    Config
	logger *slog.Logger
	srv    *http.Server
	byWork map[string]ExposedSkill // workload name → skill
	authOn bool
}

// New constructs a Server. It does not start listening.
func New(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("a2a: Backend is required")
	}
	if cfg.Listen == "" {
		cfg.Listen = ":7780"
	}
	if cfg.CardName == "" {
		cfg.CardName = "mast"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	byWork := make(map[string]ExposedSkill, len(cfg.Skills))
	for _, sk := range cfg.Skills {
		if sk.WorkloadName == "" || sk.SkillName == "" {
			return nil, fmt.Errorf("a2a: exposed skill needs both WorkloadName and SkillName (got %+v)", sk)
		}
		if _, dup := byWork[sk.WorkloadName]; dup {
			return nil, fmt.Errorf("a2a: duplicate exposed workload %q", sk.WorkloadName)
		}
		byWork[sk.WorkloadName] = sk
	}
	s := &Server{cfg: cfg, logger: logger, byWork: byWork, authOn: cfg.Validator != nil}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+WellKnownCardPath, s.handleAggregatedCard)
	mux.HandleFunc("GET /.well-known/agent-card/{name}", s.handlePerWorkloadCard)
	mux.HandleFunc("POST /a2a", s.handleRPC)
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

// Handler exposes the server's routes for mounting on an external mux or
// for tests (httptest.NewServer).
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// ListenAndServe blocks serving requests; returns http.ErrServerClosed
// on graceful shutdown.
func (s *Server) ListenAndServe() error {
	s.logger.Info("a2a server listening", "addr", s.cfg.Listen, "auth_required", s.authOn, "skills", len(s.cfg.Skills))
	return s.srv.ListenAndServe()
}

// Serve serves on an already-bound listener; returns http.ErrServerClosed
// on graceful shutdown. The daemon binds eagerly so a bad bind address
// fails startup rather than a background goroutine (mirrors buildAttach).
func (s *Server) Serve(ln net.Listener) error {
	s.logger.Info("a2a server listening", "addr", ln.Addr().String(), "auth_required", s.authOn, "skills", len(s.cfg.Skills))
	return s.srv.Serve(ln)
}

// Shutdown attempts a graceful stop.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// Close stops the server immediately.
func (s *Server) Close() error { return s.srv.Close() }

// rpcServerRequest is the inbound JSON-RPC envelope. Unlike the client's
// rpcRequest (which mints numeric ids), the id and params are kept raw:
// the id is echoed back verbatim (JSON-RPC allows string, number, or
// null) and params are decoded per-method.
type rpcServerRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// handleRPC is the single POST /a2a JSON-RPC endpoint. Auth is
// transport-level (401/403 as HTTP status); protocol problems are
// JSON-RPC error objects returned with HTTP 200.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	// A2A-Version is advisory; honor the header contract by echoing the
	// spec line we implement so clients can detect a mismatch.
	w.Header().Set(VersionHeader, ProtocolVersion)

	principal, ok := s.authenticate(w, r)
	if !ok {
		return // authenticate wrote the 401/500
	}

	var req rpcServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, nil, errCodeParse, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		s.writeError(w, req.ID, errCodeInvalidRequest, "invalid request: jsonrpc must be \"2.0\" and method is required")
		return
	}

	switch req.Method {
	case methodTasksGet:
		s.handleTasksGet(w, r.Context(), req, principal)
	case methodTasksCancel:
		s.handleTasksCancel(w, r.Context(), req, principal)
	case methodMessageSend, methodMessageStream:
		// Stage A recognizes but does not yet serve execution; Stages
		// B/C replace this with runTurnPre-backed handling.
		s.writeError(w, req.ID, errCodeUnsupportedOp, "unsupported operation: "+req.Method+" is not yet served by this mast build")
	default:
		s.writeError(w, req.ID, errCodeMethodNotFound, "method not found: "+req.Method)
	}
}

// authenticate resolves the request's bearer token to a Principal. When
// no validator is configured it returns (nil, true) — open access. It
// writes the HTTP error and returns ok=false on failure.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	if !s.authOn {
		return nil, true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	principal, err := s.cfg.Validator.Validate(r.Context(), h[len(prefix):])
	if err != nil {
		if errors.Is(err, ErrInvalidToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return nil, false
		}
		s.logger.Error("a2a token validator failed", "error", err.Error())
		http.Error(w, "token validation failed", http.StatusInternalServerError)
		return nil, false
	}
	return principal, true
}

// authorizeTask checks the principal against the required scopes of the
// skill owning the task's workload. It writes a 403 and returns false on
// a scope failure. Open (no validator) and authenticated-only skills
// (empty Scopes) pass.
func (s *Server) authorizeTask(w http.ResponseWriter, principal *Principal, workload string) bool {
	if !s.authOn {
		return true
	}
	skill, ok := s.byWork[workload]
	if !ok {
		// The task belongs to a workload this server does not expose:
		// refuse rather than leak it under some other skill's scopes.
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	for _, want := range skill.Scopes {
		if !principal.hasScope(want) {
			http.Error(w, "forbidden: missing required scope", http.StatusForbidden)
			return false
		}
	}
	return true
}

// handleTasksGet serves tasks/get: snapshot a task's state.
func (s *Server) handleTasksGet(w http.ResponseWriter, ctx context.Context, req rpcServerRequest, principal *Principal) {
	var params taskQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.ID == "" {
		s.writeError(w, req.ID, errCodeInvalidParams, "invalid params: id is required")
		return
	}
	info, err := s.cfg.Backend.GetTask(ctx, params.ID)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			s.writeError(w, req.ID, errCodeTaskNotFound, "task not found: "+params.ID)
			return
		}
		s.logger.Error("a2a tasks/get backend failed", "task", params.ID, "error", err.Error())
		s.writeError(w, req.ID, errCodeInternal, "internal error")
		return
	}
	if !s.authorizeTask(w, principal, info.WorkloadName) {
		return
	}
	s.writeResult(w, req.ID, taskFromInfo(params.ID, info))
}

// handleTasksCancel serves tasks/cancel: route to the abort machinery.
func (s *Server) handleTasksCancel(w http.ResponseWriter, ctx context.Context, req rpcServerRequest, principal *Principal) {
	var params taskIDParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.ID == "" {
		s.writeError(w, req.ID, errCodeInvalidParams, "invalid params: id is required")
		return
	}
	// Resolve the owning workload first (for the scope check and a clean
	// not-found), then cancel. The scope check follows GetTask, so an
	// authenticated-but-underscoped caller can learn a task exists; that
	// is acceptable for the operator-adjacent A2A surface.
	info, err := s.cfg.Backend.GetTask(ctx, params.ID)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			s.writeError(w, req.ID, errCodeTaskNotFound, "task not found: "+params.ID)
			return
		}
		s.logger.Error("a2a tasks/cancel lookup failed", "task", params.ID, "error", err.Error())
		s.writeError(w, req.ID, errCodeInternal, "internal error")
		return
	}
	if !s.authorizeTask(w, principal, info.WorkloadName) {
		return
	}
	canceled, err := s.cfg.Backend.CancelTask(ctx, params.ID, "a2a tasks/cancel")
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			s.writeError(w, req.ID, errCodeTaskNotFound, "task not found: "+params.ID)
			return
		}
		s.logger.Error("a2a tasks/cancel failed", "task", params.ID, "error", err.Error())
		s.writeError(w, req.ID, errCodeInternal, "internal error")
		return
	}
	s.recordTask(info.WorkloadName, canceled.State)
	s.writeResult(w, req.ID, taskFromInfo(params.ID, canceled))
}

// recordTask emits the task-outcome metric when one is wired.
func (s *Server) recordTask(workload string, state TaskState) {
	if s.cfg.Metric != nil {
		s.cfg.Metric.A2ATask(workload, string(state))
	}
}

// taskFromInfo projects a backend snapshot into the A2A Task wire shape.
func taskFromInfo(id string, info TaskInfo) *Task {
	t := &Task{
		Kind:      "task",
		ID:        id,
		ContextID: info.ContextID,
		Status:    TaskStatus{State: info.State},
	}
	if info.StatusMessage != "" {
		t.Status.Message = &Message{
			Kind:      "message",
			Role:      "agent",
			MessageID: id + "-status",
			Parts:     []Part{{Kind: "text", Text: info.StatusMessage}},
		}
	}
	return t
}

// writeResult writes a JSON-RPC success response (HTTP 200).
func (s *Server) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		s.writeError(w, id, errCodeInternal, "internal error: result marshal")
		return
	}
	s.writeEnvelope(w, rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Result: raw})
}

// writeError writes a JSON-RPC error response (HTTP 200).
func (s *Server) writeError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	s.writeEnvelope(w, rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Error: &RPCError{Code: code, Message: msg}})
}

func (s *Server) writeEnvelope(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Error("a2a response encode failed", "error", err.Error())
	}
}

// normalizeID returns the request id to echo, defaulting a missing id to
// JSON null per JSON-RPC.
func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}
