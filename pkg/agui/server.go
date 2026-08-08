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

// This file implements mast as an AG-UI *server* (docs/ag-ui-design.md): a
// per-workload HTTP endpoint that accepts a RunAgentInput POST and streams the
// turn back as Server-Sent Events of typed run events, so a browser/app UI
// (CopilotKit et al.) can render a mast workload's turn live.
//
// Build-vs-buy (issue #84): hand-rolled over the wire types this package owns
// rather than adopting the community AG-UI Go SDK — same reasoning as the A2A
// server (#78). The SDK's runner drives turns directly and would bypass mast's
// turn chokepoint (cmd/mast runTurnPre), where budget/pause/abort/turn-lock/
// effects-outbox live. The Backend seam below mirrors a2a.Backend: this package
// never imports the runtime; the daemon wires RunAgent through runTurnPre and
// translates mast events into the AG-UI frames this server writes.
//
// The A2A pattern transfers almost wholesale; the one place it does not is
// error signaling. AG-UI has no JSON-RPC error object, so pre-stream refusals
// (auth, scope, rate limit, drain) are plain HTTP status codes (401/403/429/
// 503) decided BEFORE the SSE upgrade — a client reading the stream sees a
// clean HTTP error, never a truncated event stream. As in A2A, the backend
// emits the opening frames (RunStarted, StateSnapshot) so a backend that
// refuses before any emit is reported as a clean HTTP error rather than an
// empty stream; the server emits only the terminal frame.

package agui

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

// The auth seam (Principal, TokenValidator, ErrInvalidToken) and the
// rate-limit seam (RateLimiter, RateLimitRequest) live in pkg/serverauth so a
// single validator/limiter instance authorizes and admits both the A2A and
// AG-UI surfaces (#84). This package uses them directly.

// methodRun is the rate-limit method label for a turn-driving AG-UI run. The
// limiter buckets per (caller, workload); the method is threaded so a limiter
// can key on verb, though Stage 1 has just the one turn-driving verb.
const methodRun = "agui/run"

// ErrUnavailable marks a transiently-unavailable backend — e.g. a daemon
// draining for shutdown that refuses new work. When a Backend returns it
// before emitting any frame, the server reports HTTP 503 (retryable) rather
// than opening a stream it cannot fill.
var ErrUnavailable = errors.New("agui: backend temporarily unavailable")

// ExposedWorkload is one workload's AG-UI exposure, projected by the daemon
// from the bundle's agui: section (this package does not import pkg/workload).
type ExposedWorkload struct {
	// WorkloadName is the mast workload backing this endpoint; the backend
	// resolves it to a session and drives the turn.
	WorkloadName string

	// EndpointPath is the HTTP path the workload is served at, e.g.
	// "/agui/triage". Must start with "/". Each exposed workload owns a
	// distinct path.
	EndpointPath string

	// Description is surfaced in the discovery descriptor.
	Description string

	// InputSchema is an optional JSON-Schema-shaped hint surfaced in the
	// discovery descriptor so a client can render an input form.
	InputSchema map[string]any

	// Scopes are required to invoke this workload. Empty means the endpoint
	// needs authentication only (a valid token, no specific scope) when a
	// validator is configured, or open access when it is not.
	Scopes []string
}

// RunInput is a RunAgentInput projected onto the runtime seam. The server
// extracts it from the decoded request; the daemon converts Text into a user
// turn and runs it through the turn chokepoint. State rides along as raw JSON
// for the backend to echo as the opening StateSnapshot.
type RunInput struct {
	// WorkloadName is the target workload (the endpoint the request hit).
	WorkloadName string

	// ThreadID / RunID are the client-supplied correlation ids; the daemon
	// derives the (namespaced) mast session id from them and never trusts a
	// client-supplied raw session id.
	ThreadID string
	RunID    string

	// Text is the turn's user input (the last user message's content).
	Text string

	// State is the client-supplied shared-state document, echoed back as the
	// opening StateSnapshot; nil when absent.
	State json.RawMessage
}

// RunResult is the terminal outcome of a run, returned by the Backend after
// the turn completes. Exactly one disposition holds: Aborted (operator/client
// cancellation), Interrupted (the turn paused for human input — an honest
// Stage 1 placeholder until the full HITL lifecycle lands), or neither
// (success, with Text the final answer surfaced in RunFinished.result).
type RunResult struct {
	Text        string
	Aborted     bool
	Interrupted bool
}

// Backend drives an AG-UI run against the mast runtime. The daemon implements
// it over runTurnPre (cmd/mast/agui.go); this package never imports the
// runtime. emit is called synchronously and in order on the calling goroutine
// — the SSE handler writes each frame to the wire — so implementations need no
// locking around it. The backend emits the opening frames (RunStarted, then
// StateSnapshot) and all interior frames; the server emits the terminal frame
// from the returned RunResult. A backend that cannot start the turn (draining)
// must return ErrUnavailable BEFORE any emit, so the server can report a clean
// HTTP error instead of a truncated stream. A backend must not emit after
// returning.
type Backend interface {
	RunAgent(ctx context.Context, in RunInput, emit func(any)) (RunResult, error)
}

// RunMetric records AG-UI run outcomes. The daemon backs it with
// observability.Registry.AGUIRun; nil disables. The outcome is one of a fixed
// vocabulary (see observability.Prime): success, error, aborted, rejected.
type RunMetric interface {
	AGUIRun(workload, outcome string)
}

// Run outcome labels (fixed vocabulary shared with observability.Prime).
const (
	outcomeSuccess  = "success"
	outcomeError    = "error"
	outcomeAborted  = "aborted"
	outcomeRejected = "rejected"
)

// Config configures the AG-UI server.
type Config struct {
	// Listen is the bind address, e.g. ":7781". Used by ListenAndServe.
	Listen string

	// Exposed are the workloads served, each on its own EndpointPath. An
	// empty slice serves only the discovery endpoint; the daemon starts the
	// server only when at least one workload opts in.
	Exposed []ExposedWorkload

	// Validator authenticates every run request. Nil disables auth (dev
	// only). When set, a request without a valid bearer is refused 401 before
	// any dispatch.
	Validator serverauth.TokenValidator

	// Limiter, when non-nil, admits or refuses each run request before
	// dispatch — see serverauth.RateLimiter. Nil disables rate limiting.
	Limiter serverauth.RateLimiter

	// Backend is required.
	Backend Backend

	// Metric, when non-nil, records run outcomes.
	Metric RunMetric

	// Logger defaults to slog.Default().
	Logger *slog.Logger

	// BaseContext, when non-nil, is the context every request derives from
	// (the daemon passes its turn lifetime).
	BaseContext context.Context
}

// Server is the AG-UI HTTP server. Construct with New; serve with
// ListenAndServe or Serve.
type Server struct {
	cfg    Config
	logger *slog.Logger
	srv    *http.Server
	byPath map[string]ExposedWorkload // endpoint path → workload
	authOn bool
}

// New constructs a Server. It does not start listening.
func New(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("agui: Backend is required")
	}
	// Refuse to expose an unauthenticated AG-UI surface beyond loopback
	// (mirrors the A2A #84 / attach #376 policy): a run drives a budgeted
	// turn, so an unauthenticated non-loopback bind lets any host that can
	// reach the port spend model budget. A validator (MAST_AGUI_TOKEN) is the
	// credential gate; a loopback bind is the local-dev escape hatch. Only an
	// explicitly set Listen is checked, so the embedded/test default stays
	// constructible.
	if cfg.Listen != "" && !serverauth.IsLoopbackAddr(cfg.Listen) && cfg.Validator == nil {
		return nil, fmt.Errorf("agui: refusing to bind non-loopback address %q without authentication: "+
			"any host that can reach this port could drive budgeted turns. Set an AG-UI token "+
			"(MAST_AGUI_TOKEN) or bind a loopback address (e.g. 127.0.0.1:7781)", cfg.Listen)
	}
	if cfg.Listen == "" {
		cfg.Listen = ":7781"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	byPath := make(map[string]ExposedWorkload, len(cfg.Exposed))
	for _, ew := range cfg.Exposed {
		if ew.WorkloadName == "" || ew.EndpointPath == "" {
			return nil, fmt.Errorf("agui: exposed workload needs both WorkloadName and EndpointPath (got %+v)", ew)
		}
		if !strings.HasPrefix(ew.EndpointPath, "/") {
			return nil, fmt.Errorf("agui: workload %q endpoint path %q must start with %q", ew.WorkloadName, ew.EndpointPath, "/")
		}
		if ew.EndpointPath == DiscoveryPath {
			return nil, fmt.Errorf("agui: workload %q endpoint path %q collides with the discovery path", ew.WorkloadName, ew.EndpointPath)
		}
		if _, dup := byPath[ew.EndpointPath]; dup {
			return nil, fmt.Errorf("agui: duplicate endpoint path %q", ew.EndpointPath)
		}
		byPath[ew.EndpointPath] = ew
	}
	s := &Server{cfg: cfg, logger: logger, byPath: byPath, authOn: cfg.Validator != nil}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+DiscoveryPath, s.handleDiscovery)
	for path, ew := range byPath {
		mux.HandleFunc("POST "+path, s.handleRunFor(ew))
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

// Handler exposes the server's routes for mounting on an external mux or for
// tests (httptest.NewServer).
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// ListenAndServe blocks serving requests; returns http.ErrServerClosed on
// graceful shutdown.
func (s *Server) ListenAndServe() error {
	s.logger.Info("agui server listening", "addr", s.cfg.Listen, "auth_required", s.authOn, "workloads", len(s.cfg.Exposed))
	return s.srv.ListenAndServe()
}

// Serve serves on an already-bound listener; returns http.ErrServerClosed on
// graceful shutdown. The daemon binds eagerly so a bad bind address fails
// startup rather than a background goroutine (mirrors buildAttach / a2a).
func (s *Server) Serve(ln net.Listener) error {
	s.logger.Info("agui server listening", "addr", ln.Addr().String(), "auth_required", s.authOn, "workloads", len(s.cfg.Exposed))
	return s.srv.Serve(ln)
}

// Shutdown attempts a graceful stop.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// Close stops the server immediately.
func (s *Server) Close() error { return s.srv.Close() }

// handleRunFor binds one exposed workload to its POST handler.
func (s *Server) handleRunFor(ew ExposedWorkload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.handleRun(w, r, ew) }
}

// handleRun accepts a RunAgentInput POST for one workload and streams the turn
// back as SSE. Every refusal (auth, decode, scope, rate limit, missing
// Flusher, pre-stream drain) is decided BEFORE the SSE upgrade and rides a
// plain HTTP status code, so a client never sees a truncated event stream. The
// backend emits the opening + interior frames; this handler emits only the
// terminal frame from the returned RunResult.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request, ew ExposedWorkload) {
	defer func() { _ = r.Body.Close() }()

	principal, ok := s.authenticate(w, r)
	if !ok {
		return // authenticate wrote the 401/500
	}

	// Adopt any W3C trace context the caller propagated so the turn's span
	// tree parents under the caller's span. No-op when tracing is disabled.
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

	var in RunAgentInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: malformed RunAgentInput", http.StatusBadRequest)
		return
	}
	text := lastUserText(in.Messages)
	if text == "" {
		http.Error(w, "bad request: no user message text (Stage 1 requires a text user message)", http.StatusBadRequest)
		return
	}

	if !s.authorize(w, principal, ew) {
		return
	}
	// Admission control gates the turn-driving run before the SSE upgrade, so
	// a refusal is a plain HTTP 429 with an advisory Retry-After.
	if !s.rateLimit(w, ctx, principal, ew.WorkloadName) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "internal error: streaming unsupported (no http.Flusher)", http.StatusInternalServerError)
		return
	}
	// writeSSEEvent arms a per-frame write deadline; clear it before returning
	// so a leftover absolute deadline cannot break a later response on the
	// same keep-alive connection.
	defer func() { _ = http.NewResponseController(w).SetWriteDeadline(time.Time{}) }()

	// A stalled or vanished consumer must not pin the turn: emit runs inside
	// runTurnPre on the turn goroutine, holding the per-session lock and the
	// drain in-flight bracket. Cancel the turn ctx when a frame write fails so
	// the turn aborts and releases those instead of blocking.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	// Upgrade to SSE lazily, on the first emitted frame: a backend that
	// refuses before any emit (drain race) is then reported as a clean HTTP
	// error rather than an empty event stream.
	var started bool
	emit := func(ev any) {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no") // disable nginx-style proxy buffering
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if err := s.writeSSEEvent(w, flusher, ev); err != nil {
			cancelStream()
		}
	}

	result, err := s.cfg.Backend.RunAgent(streamCtx, RunInput{
		WorkloadName: ew.WorkloadName,
		ThreadID:     in.ThreadID,
		RunID:        in.RunID,
		Text:         text,
		State:        in.State,
	}, emit)
	if err != nil {
		if !started {
			// Nothing streamed yet: a plain HTTP error is still valid.
			if errors.Is(err, ErrUnavailable) {
				s.recordRun(ew.WorkloadName, outcomeRejected)
				http.Error(w, "server temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			s.logger.Error("agui run failed", "workload", ew.WorkloadName, "error", err.Error())
			s.recordRun(ew.WorkloadName, outcomeError)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// The stream is already open: close it with a terminal RunError so the
		// client sees an ordered end rather than a truncated stream. Leak no
		// server-side detail in the client-facing message.
		s.logger.Error("agui run failed mid-stream", "workload", ew.WorkloadName, "error", err.Error())
		s.recordRun(ew.WorkloadName, outcomeError)
		emit(NewRunError("internal error", RunErrorInternal))
		return
	}

	// Terminal frame from the run's disposition. Exactly one holds.
	switch {
	case result.Aborted:
		s.recordRun(ew.WorkloadName, outcomeAborted)
		emit(NewRunError("run aborted", RunErrorAborted))
	case result.Interrupted:
		// Honest placeholder: the turn paused for human input. The full
		// interrupt/resume lifecycle is a follow-on stage; never fabricate a
		// success here.
		s.recordRun(ew.WorkloadName, outcomeError)
		emit(NewRunError("run interrupted: awaiting human input (resume is not yet served)", RunErrorInterrupt))
	default:
		s.recordRun(ew.WorkloadName, outcomeSuccess)
		emit(NewRunFinished(in.ThreadID, in.RunID, resultJSON(result.Text)))
	}
}

// authenticate resolves the request's bearer token to a Principal. When no
// validator is configured it returns (nil, true) — open access. It writes the
// HTTP error and returns ok=false on failure.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*serverauth.Principal, bool) {
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
		if errors.Is(err, serverauth.ErrInvalidToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return nil, false
		}
		s.logger.Error("agui token validator failed", "error", err.Error())
		http.Error(w, "token validation failed", http.StatusInternalServerError)
		return nil, false
	}
	return principal, true
}

// authorize checks the principal against the exposed workload's required
// scopes. It writes a 403 and returns false on a scope failure. Open (no
// validator) and authenticated-only workloads (empty Scopes) pass.
func (s *Server) authorize(w http.ResponseWriter, principal *serverauth.Principal, ew ExposedWorkload) bool {
	if !s.authOn {
		return true
	}
	for _, want := range ew.Scopes {
		if !principal.HasScope(want) {
			http.Error(w, "forbidden: missing required scope", http.StatusForbidden)
			return false
		}
	}
	return true
}

// rateLimit admits a run through the configured limiter, keyed by the caller
// (principal) and target workload. On a refusal it records "rejected", sets an
// advisory Retry-After, and writes HTTP 429; it returns false. A nil limiter
// admits. A nil principal (unauthenticated endpoint) is still rate limited —
// with empty Subject/Tenant, so all unauthenticated callers share one bucket
// per workload.
func (s *Server) rateLimit(w http.ResponseWriter, ctx context.Context, principal *serverauth.Principal, workload string) bool {
	if s.cfg.Limiter == nil {
		return true
	}
	req := serverauth.RateLimitRequest{Workload: workload, Method: methodRun}
	if principal != nil {
		req.Subject = principal.Subject
		req.Tenant = principal.Tenant
	}
	ok, retryAfter := s.cfg.Limiter.Allow(ctx, req)
	if ok {
		return true
	}
	s.recordRun(workload, outcomeRejected)
	if retryAfter > 0 {
		// Round the advisory hint UP: flooring a 1.9s wait to "1" would tell a
		// compliant client to retry before a token is available.
		secs := int(math.Ceil(retryAfter.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	http.Error(w, "rate limit exceeded; retry later", http.StatusTooManyRequests)
	return false
}

// sseWriteTimeout bounds a single SSE frame write. Reset per frame, so a
// legitimately long turn is fine — only a consumer that has stopped draining
// its socket trips it. Generous for a slow-but-live network; short enough that
// a stalled reader cannot pin the turn indefinitely.
const sseWriteTimeout = 30 * time.Second

// writeSSEEvent frames one event as a bare `data: <json>\n\n` SSE block (no
// JSON-RPC envelope — AG-UI events are the payload), then flushes. A marshal
// failure is logged and the frame skipped (returning nil) rather than killing
// the stream. A write failure — client disconnected, or the per-frame deadline
// lapsed on a stalled consumer — is returned so the caller can cancel the turn
// instead of blocking further emits on a socket that will never drain.
func (s *Server) writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, ev any) error {
	buf, err := json.Marshal(ev)
	if err != nil {
		s.logger.Error("agui event marshal failed", "error", err.Error())
		return nil
	}
	// Best-effort per-frame deadline: unsupported writers (rare) just skip it.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
		s.logger.Debug("agui stream write failed (client gone or stalled)", "error", err.Error())
		return err
	}
	flusher.Flush()
	return nil
}

// recordRun emits the run-outcome metric when one is wired.
func (s *Server) recordRun(workload, outcome string) {
	if s.cfg.Metric != nil {
		s.cfg.Metric.AGUIRun(workload, outcome)
	}
}

// lastUserText returns the content of the last user-role message — the turn's
// input. AG-UI clients send the full thread each run; the newest user message
// is the new turn. Returns "" when there is no user message with content.
func lastUserText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// resultJSON encodes a run's final answer as the JSON value carried in
// RunFinished.result. A marshal failure (impossible for a string) degrades to
// a JSON null rather than dropping the frame.
func resultJSON(text string) json.RawMessage {
	b, err := json.Marshal(text)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
