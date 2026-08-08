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
// The endpoint serves the card, tasks/get, tasks/cancel, message/send
// (synchronous, Stage B) and message/stream (SSE, Stage C) behind
// pluggable bearer auth and an optional rate limiter.

package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/go-steer/mast/pkg/serverauth"
)

// The auth seam — Principal, TokenValidator, StaticBearerValidator,
// ErrInvalidToken — and the rate-limit seam (RateLimiter, RateLimitRequest,
// TokenBucketLimiter) live in pkg/serverauth so a single validator/limiter
// instance authorizes both the A2A and AG-UI surfaces (#84). This package
// re-exports them as aliases (serverauth_alias.go) so a2a.Principal etc.
// keep working unchanged.

// ErrTaskNotFound marks an unknown task id; the server maps it to the
// A2A TaskNotFound JSON-RPC error (-32001).
var ErrTaskNotFound = errors.New("a2a: task not found")

// ErrUnavailable marks a transiently-unavailable backend — e.g. a server
// draining for shutdown that refuses new work. The server maps it to a
// server-error JSON-RPC code (-32000) so a caller reads it as retryable,
// not as an internal fault (-32603). A Backend returns it (wrapped) from
// SubmitMessage.
var ErrUnavailable = errors.New("a2a: backend temporarily unavailable")

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

	// Output is the agent's answer for a completed task; when non-empty
	// it surfaces as a text artifact on the returned Task (Stage B
	// message/send). Empty for read/cancel snapshots.
	Output string
}

// SubmitParams is a message/send request projected onto the runtime
// seam. The server extracts it from the A2A Message; the daemon converts
// it into a mast turn (Text → user message) and runs it through the turn
// chokepoint. Data/file parts are text-only for Stage B (docs/a2a-design.md).
type SubmitParams struct {
	// TaskID continues an existing task (== session id) when set; empty
	// mints a fresh task.
	TaskID string

	// ContextID groups related messages; carried onto the task snapshot.
	ContextID string

	// Text is the joined text of the inbound message's parts.
	Text string
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

	// SubmitMessage runs a message/send turn through the mast turn
	// chokepoint and returns the resolved task id and its terminal
	// snapshot. Continuing a task whose id is unknown returns
	// ErrTaskNotFound; a turn that runs but errors is reported through the
	// snapshot's State (failed / canceled / input-required), not an error
	// return. The ctx carries any propagated caller trace context.
	SubmitMessage(ctx context.Context, p SubmitParams) (taskID string, info TaskInfo, err error)

	// StreamMessage runs a message/stream turn through the same chokepoint
	// as SubmitMessage, emitting streaming updates through emit as the turn
	// progresses. emit is called synchronously and in order on the calling
	// goroutine — the SSE handler writes each frame to the wire — so
	// implementations need no locking around it. The first emit is the
	// initial *Task snapshot; subsequent emits are *TaskStatusUpdateEvent
	// progress updates (final=false). The returned task id and terminal
	// snapshot let the server emit the closing artifact + a final
	// status-update. Error semantics mirror SubmitMessage (ErrTaskNotFound
	// / ErrUnavailable before any emit; a turn that runs but fails is
	// reported through the snapshot State). A backend must not emit after
	// returning.
	StreamMessage(ctx context.Context, p SubmitParams, emit func(any)) (taskID string, info TaskInfo, err error)
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

	// Limiter, when non-nil, admits or refuses each turn-driving request
	// (message/send) before dispatch — see RateLimiter. Nil disables rate
	// limiting. Control-plane verbs (tasks/get, tasks/cancel) are never
	// gated.
	Limiter RateLimiter

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
	// Refuse to expose an unauthenticated A2A surface beyond loopback
	// (mirrors the attach #376 policy). tasks/cancel is destructive — it
	// drives the same terminal-abort path as the /abort door — so an
	// unauthenticated non-loopback bind lets any host that can reach the
	// port cancel sessions (and read task state) by id. A validator
	// (MAST_A2A_TOKEN) is the credential gate; a loopback bind is the
	// local-dev escape hatch. Only an explicitly set Listen is checked,
	// so the embedded/test default stays constructible.
	if cfg.Listen != "" && !serverauth.IsLoopbackAddr(cfg.Listen) && cfg.Validator == nil {
		return nil, fmt.Errorf("a2a: refusing to bind non-loopback address %q without authentication: "+
			"any host that can reach this port could cancel sessions (tasks/cancel) and read task state "+
			"(tasks/get). Set an A2A token (MAST_A2A_TOKEN) or bind a loopback address (e.g. 127.0.0.1:7780)", cfg.Listen)
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

	// Adopt any W3C trace context the caller propagated (traceparent /
	// baggage) so the turn's span tree parents under the caller's span
	// for end-to-end distributed tracing. No-op when tracing is disabled:
	// the global propagator defaults to a no-op that returns ctx unchanged.
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

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
		s.handleTasksGet(w, ctx, req, principal)
	case methodTasksCancel:
		s.handleTasksCancel(w, ctx, req, principal)
	case methodMessageSend:
		s.handleMessageSend(w, ctx, req, principal)
	case methodMessageStream:
		s.handleMessageStream(w, ctx, req, principal)
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
		if !principal.HasScope(want) {
			http.Error(w, "forbidden: missing required scope", http.StatusForbidden)
			return false
		}
	}
	return true
}

// rateLimit admits a turn-driving request through the configured limiter,
// keyed by the caller (principal) and target workload. On a refusal it
// records a "rejected" outcome, sets an advisory Retry-After header, and
// writes the A2A retryable error (-32000); it returns false so the caller
// stops. A nil limiter admits. A nil principal (unauthenticated endpoint)
// is still rate limited — with empty Subject/Tenant, so all unauthenticated
// callers share one bucket per workload. The method is threaded so a
// limiter can key on verb, though v0.2 only gates message/send.
func (s *Server) rateLimit(w http.ResponseWriter, ctx context.Context, id json.RawMessage, principal *Principal, workload, method string) bool {
	if s.cfg.Limiter == nil {
		return true
	}
	req := RateLimitRequest{Workload: workload, Method: method}
	if principal != nil {
		req.Subject = principal.Subject
		req.Tenant = principal.Tenant
	}
	ok, retryAfter := s.cfg.Limiter.Allow(ctx, req)
	if ok {
		return true
	}
	s.recordTask(workload, TaskStateRejected)
	msg := "rate limit exceeded; retry later"
	if retryAfter > 0 {
		// Round the advisory hint UP: flooring a 1.9s wait to "1" would tell
		// a compliant client to retry before a token is available.
		secs := int(math.Ceil(retryAfter.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		msg = fmt.Sprintf("rate limit exceeded; retry after %ds", secs)
	}
	s.writeError(w, id, errCodeUnavailable, msg)
	return false
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

// handleMessageSend serves message/send: run a turn through the mast
// chokepoint and return the resulting task. Execution is synchronous
// (the v0.2 client blocks; runTurnPre is synchronous), so the reply is a
// terminal Task rather than a streamed one.
func (s *Server) handleMessageSend(w http.ResponseWriter, ctx context.Context, req rpcServerRequest, principal *Principal) {
	var params messageSendParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Message == nil {
		s.writeError(w, req.ID, errCodeInvalidParams, "invalid params: message is required")
		return
	}
	text := textOf(params.Message.Parts)
	if text == "" {
		s.writeError(w, req.ID, errCodeInvalidParams, "invalid params: message has no text parts (Stage B is text-only)")
		return
	}

	// Resolve the target workload for the scope check. A message that
	// continues an existing task inherits its workload; a fresh message
	// routes to the single exposed workload (routing multiple skills by
	// message metadata is a later stage).
	workload, ok := s.resolveSendWorkload(w, ctx, req.ID, params.Message)
	if !ok {
		return
	}
	if !s.authorizeTask(w, principal, workload) {
		return
	}
	// Admission control: gate the turn-driving verb (message/send) only —
	// it is what consumes model budget. A refusal is retryable (-32000).
	if !s.rateLimit(w, ctx, req.ID, principal, workload, methodMessageSend) {
		return
	}

	taskID, info, err := s.cfg.Backend.SubmitMessage(ctx, SubmitParams{
		TaskID:    params.Message.TaskID,
		ContextID: params.Message.ContextID,
		Text:      text,
	})
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			s.writeError(w, req.ID, errCodeTaskNotFound, "task not found: "+params.Message.TaskID)
			return
		}
		if errors.Is(err, ErrUnavailable) {
			// Draining or otherwise transiently unavailable: retryable, not
			// an internal fault.
			s.writeError(w, req.ID, errCodeUnavailable, "server temporarily unavailable")
			return
		}
		s.logger.Error("a2a message/send failed", "task", taskID, "workload", workload, "error", err.Error())
		s.writeError(w, req.ID, errCodeInternal, "internal error")
		return
	}
	s.recordTask(workload, info.State)
	s.writeResult(w, req.ID, taskFromInfo(taskID, info))
}

// resolveSendWorkload resolves the target workload for a message-driving
// verb (message/send, message/stream): a continuation inherits its task's
// workload; a fresh message routes to the single exposed skill. It writes
// the JSON-RPC error and returns ok=false on failure.
func (s *Server) resolveSendWorkload(w http.ResponseWriter, ctx context.Context, id json.RawMessage, msg *Message) (string, bool) {
	if tid := msg.TaskID; tid != "" {
		info, err := s.cfg.Backend.GetTask(ctx, tid)
		if err != nil {
			if errors.Is(err, ErrTaskNotFound) {
				s.writeError(w, id, errCodeTaskNotFound, "task not found: "+tid)
				return "", false
			}
			s.logger.Error("a2a message task lookup failed", "task", tid, "error", err.Error())
			s.writeError(w, id, errCodeInternal, "internal error")
			return "", false
		}
		return info.WorkloadName, true
	}
	if len(s.byWork) != 1 {
		s.writeError(w, id, errCodeUnsupportedOp,
			"message requires exactly one exposed skill on this endpoint (multi-skill routing is not yet served)")
		return "", false
	}
	var workload string
	for wn := range s.byWork {
		workload = wn
	}
	return workload, true
}

// handleMessageStream serves message/stream: run a turn through the mast
// chokepoint and stream its progress as Server-Sent Events, one JSON-RPC
// response per SSE `data:` frame (docs/a2a-design.md "Stage C"). The
// backend emits the initial Task snapshot and per-event working
// status-updates through emit; this handler frames each on the wire and,
// on completion, emits the closing artifact + a final status-update that
// ends the stream.
//
// Auth, scope, and rate-limit refusals are decided BEFORE the SSE upgrade,
// so they ride a normal JSON-RPC error response — a caller reading the
// stream sees a clean error, not a truncated event stream. Because the
// turn path runs StreamingModeNone, updates are message-granular (one per
// model response), not token deltas; token-level streaming is a follow-on.
func (s *Server) handleMessageStream(w http.ResponseWriter, ctx context.Context, req rpcServerRequest, principal *Principal) {
	var params messageSendParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Message == nil {
		s.writeError(w, req.ID, errCodeInvalidParams, "invalid params: message is required")
		return
	}
	text := textOf(params.Message.Parts)
	if text == "" {
		s.writeError(w, req.ID, errCodeInvalidParams, "invalid params: message has no text parts (streaming is text-only)")
		return
	}
	workload, ok := s.resolveSendWorkload(w, ctx, req.ID, params.Message)
	if !ok {
		return
	}
	if !s.authorizeTask(w, principal, workload) {
		return
	}
	// Admission control gates the turn-driving verb before the SSE upgrade,
	// so a refusal is a normal JSON-RPC error (-32000). message/stream
	// consumes model budget exactly as message/send does, so it shares the
	// same (caller, workload) bucket.
	if !s.rateLimit(w, ctx, req.ID, principal, workload, methodMessageStream) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, req.ID, errCodeInternal, "internal error: streaming unsupported (no http.Flusher)")
		return
	}
	// writeSSEEvent arms a per-frame write deadline on the connection; clear it
	// before returning so a leftover absolute deadline cannot break a later
	// response on the same keep-alive connection (the server sets no
	// WriteTimeout that would otherwise reset it per request).
	defer func() { _ = http.NewResponseController(w).SetWriteDeadline(time.Time{}) }()

	// A stalled or vanished consumer must not pin the turn: emit runs inside
	// runTurnPre on the turn goroutine, holding the per-session lock and the
	// drain in-flight bracket, so a write that blocks on a full socket buffer
	// would survive drain-window cancellation. Cancel the turn ctx when a frame
	// write fails (deadline exceeded or client gone) so the turn aborts and
	// releases those instead of blocking.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	// Upgrade to SSE lazily, on the first emitted frame: a backend that
	// refuses before any emit (drain race, unknown continuation) is then
	// reported as a normal JSON-RPC error rather than an empty event stream.
	var started bool
	emit := func(result any) {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no") // disable nginx-style proxy buffering
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if err := s.writeSSEEvent(w, flusher, req.ID, result); err != nil {
			cancelStream()
		}
	}

	taskID, info, err := s.cfg.Backend.StreamMessage(streamCtx, SubmitParams{
		TaskID:    params.Message.TaskID,
		ContextID: params.Message.ContextID,
		Text:      text,
	}, emit)
	if err != nil {
		if !started {
			// Nothing streamed yet: a normal JSON-RPC error is still valid.
			switch {
			case errors.Is(err, ErrTaskNotFound):
				s.writeError(w, req.ID, errCodeTaskNotFound, "task not found: "+params.Message.TaskID)
			case errors.Is(err, ErrUnavailable):
				s.writeError(w, req.ID, errCodeUnavailable, "server temporarily unavailable")
			default:
				s.logger.Error("a2a message/stream failed", "task", taskID, "workload", workload, "error", err.Error())
				s.writeError(w, req.ID, errCodeInternal, "internal error")
			}
			return
		}
		// The stream is already open: close it with a terminal failed
		// status-update so the client sees an ordered end rather than a
		// truncated stream (the drain-after-initial-Task race, mainly).
		s.logger.Error("a2a message/stream failed mid-stream", "task", taskID, "workload", workload, "error", err.Error())
		emit(&TaskStatusUpdateEvent{
			Kind:      "status-update",
			TaskID:    taskID,
			ContextID: info.ContextID,
			Status:    TaskStatus{State: TaskStateFailed},
			Final:     true,
		})
		return
	}

	s.recordTask(workload, info.State)
	// Closing bookend: the output artifact (when any), then the final
	// status-update that ends the stream. Both reuse the terminal snapshot
	// the backend returned — the same projection message/send replies with.
	if info.Output != "" {
		emit(&TaskArtifactUpdateEvent{
			Kind:      "artifact-update",
			TaskID:    taskID,
			ContextID: info.ContextID,
			Artifact: Artifact{
				ArtifactID: taskID + "-result",
				Name:       "result",
				Parts:      []Part{{Kind: "text", Text: info.Output}},
			},
			LastChunk: true,
		})
	}
	// The final frame carries the same projection message/send would reply
	// with. It is usually terminal (completed/failed/canceled) or
	// input-required, but a turn the chokepoint refused because the session
	// was gate-paused (operator/timed, no pending interrupt) projects as
	// working — a non-terminal yet honest state. Final still ends the stream:
	// the turn returned, so there is nothing more to emit, and fabricating a
	// terminal state would lie. A client seeing working+final should poll
	// tasks/get to follow the task as it resumes.
	final := &TaskStatusUpdateEvent{
		Kind:      "status-update",
		TaskID:    taskID,
		ContextID: info.ContextID,
		Status:    TaskStatus{State: info.State},
		Final:     true,
	}
	if info.StatusMessage != "" {
		final.Status.Message = &Message{
			Kind:      "message",
			Role:      "agent",
			MessageID: taskID + "-status",
			Parts:     []Part{{Kind: "text", Text: info.StatusMessage}},
		}
	}
	emit(final)
}

// sseWriteTimeout bounds a single SSE frame write. It is reset per frame,
// so a legitimately long turn (large gaps between events) is fine — only a
// consumer that has stopped draining its socket, letting the kernel buffer
// fill, trips it. Generous enough for a slow-but-live network; short enough
// that a stalled reader cannot pin the turn indefinitely.
const sseWriteTimeout = 30 * time.Second

// writeSSEEvent frames one streaming result as a JSON-RPC response inside a
// single SSE `data:` block, then flushes. A marshal failure is logged and
// the frame skipped (returning nil) rather than killing the stream. A write
// failure — the client disconnected, or the per-frame deadline lapsed on a
// stalled consumer — is returned so the caller can cancel the turn instead of
// blocking further emits on a socket that will never drain.
func (s *Server) writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		s.logger.Error("a2a stream result marshal failed", "error", err.Error())
		return nil
	}
	buf, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Result: raw})
	if err != nil {
		s.logger.Error("a2a stream envelope marshal failed", "error", err.Error())
		return nil
	}
	// Best-effort per-frame deadline: unsupported writers (rare) just skip it.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
		s.logger.Debug("a2a stream write failed (client gone or stalled)", "error", err.Error())
		return err
	}
	flusher.Flush()
	return nil
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
	if info.Output != "" {
		t.Artifacts = append(t.Artifacts, Artifact{
			ArtifactID: id + "-result",
			Name:       "result",
			Parts:      []Part{{Kind: "text", Text: info.Output}},
		})
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
