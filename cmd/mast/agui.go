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

package main

// AG-UI-server wiring for the daemon (docs/ag-ui-design.md). pkg/agui owns the
// wire protocol, auth, and HTTP+SSE surface but never imports the runtime; this
// file supplies the Backend seam — RunAgent drives one turn through the same
// runTurnPre chokepoint every other turn kind funnels through (inheriting the
// turn lock, cancel registry, abort/gate-pause refusal, budget meter, watchdog,
// and effects outbox), translating mast events into the AG-UI SSE frames the
// server writes. It also projects the bundle's agui: section into the exposed
// endpoints.
//
// Stage 1 is server core: the happy-path run stream, discovery, auth, and rate
// limiting. The HITL interrupt/resume lifecycle, the agui:// federation client,
// and per-key state deltas are follow-on stages (an interrupted turn maps to an
// honest RunError{interrupt}, never a fabricated success).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/agui"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/serverauth"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// aguiBackend implements agui.Backend over the daemon's turn stack. It mirrors
// a2aBackend's seam objects but has no task registry: an AG-UI run streams
// synchronously and its terminal disposition rides the SSE stream, so there is
// no out-of-band task-state read to reconcile.
type aguiBackend struct {
	store        *transcript.Store
	obs          *observability.Registry
	tracker      *turnTracker
	logger       *slog.Logger
	workloadName string

	// Turn-execution seams — the same objects the inject/attach/resume/a2a
	// paths thread into runTurnPre.
	r         *runner.Runner
	meters    *meterPool
	wds       *watchdogPool
	turnLocks *sessionTurnLocks
	bundle    *workload.Bundle
}

// aguiSessionPrefix namespaces every AG-UI-derived session id. Like the
// "a2a-" task prefix it both keeps AG-UI sessions clear of other surfaces'
// namespaces (inject "incident-*", attach, autoresume) and marks a session as
// one this surface owns (see isAGUISessionID).
const aguiSessionPrefix = "agui-"

// sessionModel returns the workload's configured AG-UI session model,
// defaulting to per_thread (the dominant chat case: one continuing
// conversation per thread).
func (b *aguiBackend) sessionModel() string {
	if b.bundle != nil && b.bundle.AGUI.SessionModel != "" {
		return b.bundle.AGUI.SessionModel
	}
	return workload.AGUISessionPerThread
}

// sessionIDFor derives the mast session id for a run from the client-supplied
// threadId/runId, always namespaced under the AG-UI prefix. The client never
// supplies a raw session id — the daemon derives it — so a caller cannot drive
// a turn into another surface's session by presenting its id (structural
// separation, stronger than a runtime check). When the correlation id the
// session model keys on is absent, a fresh owned session is minted rather than
// colliding every id-less caller onto one shared session.
func (b *aguiBackend) sessionIDFor(threadID, runID string) string {
	switch b.sessionModel() {
	case workload.AGUISessionPerRun:
		if runID != "" {
			return aguiSessionPrefix + "run-" + runID
		}
	default:
		if threadID != "" {
			return aguiSessionPrefix + "thread-" + threadID
		}
	}
	return mintID(aguiSessionPrefix)
}

// isAGUISessionID reports whether id names a session this AG-UI surface owns.
// Belt-and-suspenders to the structural derivation: it rejects a session id
// that a crafted threadId/runId pushed into the reserved ops-row namespace,
// mirroring isA2ATaskID.
func isAGUISessionID(id string) bool {
	return strings.HasPrefix(id, aguiSessionPrefix) && !transcript.IsReservedSessionID(id)
}

// RunAgent drives one AG-UI run through runTurnPre and streams the turn back as
// AG-UI frames. It emits the opening frames (RunStarted, then a StateSnapshot
// echoing the client's input state) and, via the translating onEvent, all
// interior text/tool frames; the server emits the terminal frame from the
// returned RunResult. A pre-turn refusal (draining, or a crafted id that
// escaped the owned namespace) returns before any emit so the server reports a
// clean HTTP status rather than an orphaned stream.
func (b *aguiBackend) RunAgent(ctx context.Context, in agui.RunInput, emit func(any)) (agui.RunResult, error) {
	// Drain gate: refuse new work once shutdown has begun, BEFORE any emit
	// (mirrors the inject handler and a2a). ErrUnavailable → HTTP 503.
	if b.tracker.isDraining() {
		return agui.RunResult{}, fmt.Errorf("agui: server draining, not accepting new runs: %w", agui.ErrUnavailable)
	}

	sessionID := b.sessionIDFor(in.ThreadID, in.RunID)
	if !isAGUISessionID(sessionID) {
		// A crafted threadId/runId collided with the reserved ops-row
		// namespace; refuse before any emit rather than drive a turn into a
		// reserved row (which would corrupt its marker storage).
		return agui.RunResult{}, fmt.Errorf("agui: derived session id %q is not addressable", sessionID)
	}

	// Time the executed run for the duration histogram; only runs that pass
	// the pre-turn refusals above are measured, so the histogram reflects real
	// turn wallclock rather than fast rejections.
	start := time.Now()
	defer func() { b.obs.AGUIRunDuration(b.workloadName, time.Since(start).Seconds()) }()

	// Opening frames (mirrors A2A's initial Task snapshot as its first emit):
	// RunStarted, then a StateSnapshot echoing the client's input state, or an
	// empty object when absent. Per-key StateDelta emission is a follow-on.
	emit(agui.NewRunStarted(in.ThreadID, in.RunID))
	snapshot := in.State
	if len(snapshot) == 0 {
		snapshot = json.RawMessage("{}")
	}
	emit(agui.NewStateSnapshot(snapshot))

	// Same wallclock ceiling as the inject/resume/a2a paths (#47).
	if b.bundle != nil && b.bundle.Budget.MaxWallclockSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(b.bundle.Budget.MaxWallclockSeconds)*time.Second)
		defer cancel()
	}

	em := &aguiEmitter{emit: emit}
	msg := genai.NewContentFromText(in.Text, genai.RoleUser)
	err := runTurnPre(ctx, b.r, b.logger, b.store, b.meters, b.wds, b.obs, b.tracker, b.turnLocks,
		b.workloadName, sessionID, msg, "agui:run", nil, em.onEvent)

	return b.classifyRun(ctx, sessionID, em, err)
}

// classifyRun maps a runTurnPre outcome onto an AG-UI run disposition. The
// stream is already open (RunStarted emitted), so a genuine failure surfaces as
// a returned error the server closes the stream with (RunError{internal}, real
// error logged there, no detail leaked); a chokepoint refusal projects the
// session's durable state onto Aborted/Interrupted; a clean finish is success,
// or Interrupted when the turn paused for human input.
func (b *aguiBackend) classifyRun(ctx context.Context, sessionID string, em *aguiEmitter, err error) (agui.RunResult, error) {
	// The turn ctx may already be canceled here (a mid-flight abort or a
	// wallclock trip cancels it), so project durable state through a detached
	// context — the marker read must not fail merely because the turn's ctx is
	// done.
	readCtx := context.WithoutCancel(ctx)
	switch {
	case err == nil:
		// A HITL pause finishes the turn with err==nil and the interrupt signal
		// captured; report it honestly (→ RunError{interrupt}) rather than a
		// success. The full interrupt/resume lifecycle is a follow-on stage.
		return agui.RunResult{Text: em.lastText, Interrupted: em.interrupted}, nil
	case errors.Is(err, inject.ErrConflict):
		// The chokepoint refused the turn: the session is aborted or paused.
		// Project its durable state rather than a generic error.
		if res, ok := b.dispositionFromStore(readCtx, sessionID); ok {
			return res, nil
		}
		return agui.RunResult{Aborted: true}, nil
	default:
		// Runner error, budget trip, wallclock/ctx cancellation, or the narrow
		// post-pre-check drain race (inject.ErrUnavailable while blocked on the
		// turn lock). A mid-flight operator abort cancels the turn ctx from
		// OUTSIDE the chokepoint (the /abort door writes the durable marker, then
		// sweeps the in-flight turn's cancel handle), so runTurnPre returns a
		// plain context cancellation here rather than ErrConflict. The abort
		// marker is the ground truth — the AG-UI analogue of A2A's sticky-Canceled
		// registry reconciliation — so consult it and report Aborted rather than a
		// misleading internal error. Budget trips and wallclock ceilings also
		// cancel the ctx but write no abort marker, so they still surface as the
		// generic RunError{internal} the server closes the open stream with; never
		// fabricate a success.
		if d, gerr := b.store.Get(readCtx, "", sessionID); gerr == nil && d.State == transcript.StateAborted {
			return agui.RunResult{Aborted: true}, nil
		}
		return agui.RunResult{}, err
	}
}

// dispositionFromStore projects a chokepoint-refused session's durable state
// onto an AG-UI run disposition. ok is false when the read failed or the state
// is not one the caller maps (the caller then falls back to Aborted, the safe
// close for a refused turn).
func (b *aguiBackend) dispositionFromStore(ctx context.Context, sessionID string) (agui.RunResult, bool) {
	d, err := b.store.Get(ctx, "", sessionID)
	if err != nil {
		return agui.RunResult{}, false
	}
	switch {
	case d.State == transcript.StateAborted:
		return agui.RunResult{Aborted: true}, true
	case d.State == transcript.StatePaused && len(d.PendingInterruptIDs) > 0:
		return agui.RunResult{Interrupted: true}, true
	case d.State == transcript.StatePaused:
		// Gate-only pause (operator/timed hold): the run was refused. Aborted is
		// the closest Stage 1 disposition; a dedicated "refused" code is a
		// follow-on.
		return agui.RunResult{Aborted: true}, true
	}
	return agui.RunResult{}, false
}

// aguiEmitter translates a turn's mast event stream into AG-UI frames and
// accumulates the terminal signals the run disposition needs. Its onEvent runs
// synchronously inside runTurnPre's event loop, on the same goroutine as
// RunAgent, so it needs no locking.
type aguiEmitter struct {
	emit func(any)
	seq  int

	// lastText is the final model-authored answer, surfaced in
	// RunFinished.result; interrupted records a HITL pause signal.
	lastText    string
	interrupted bool
}

// nextID mints a per-run-unique id for a synthesized message/tool call. mast
// runs StreamingModeNone (one whole model response per event), so ids are
// per-event, not per-token.
func (e *aguiEmitter) nextID(kind string) string {
	e.seq++
	return fmt.Sprintf("agui-%s-%d", kind, e.seq)
}

// onEvent translates one runner event into AG-UI frames: model-authored text
// becomes a TextMessage triad (start → one content frame with the whole text →
// end, since StreamingModeNone is message-granular); each model FunctionCall
// becomes a ToolCall start/args/end triple parented to that message; each
// FunctionResponse (which arrives on non-model events) becomes a ToolCallResult.
// A RequestedInput or an unanswered long-running tool marks the run interrupted.
func (e *aguiEmitter) onEvent(ev *session.Event) {
	if ev == nil {
		return
	}
	if ev.RequestedInput != nil || len(ev.LongRunningToolIDs) > 0 {
		e.interrupted = true
	}
	if ev.Content == nil {
		return
	}
	model := ev.Content.Role == genai.RoleModel

	// Assistant text: one whole message per model event. User/tool echoes on
	// the stream are not re-emitted as assistant text.
	var msgID string
	if model {
		var sb strings.Builder
		for _, part := range ev.Content.Parts {
			if part != nil && part.Text != "" {
				sb.WriteString(part.Text)
			}
		}
		if sb.Len() > 0 {
			msgID = e.nextID("msg")
			e.emit(agui.NewTextMessageStart(msgID))
			e.emit(agui.NewTextMessageContent(msgID, sb.String()))
			e.emit(agui.NewTextMessageEnd(msgID))
			e.lastText = sb.String()
		}
	}

	for _, part := range ev.Content.Parts {
		if part == nil {
			continue
		}
		if model && part.FunctionCall != nil {
			e.emitToolCall(part.FunctionCall, msgID)
		}
		if part.FunctionResponse != nil {
			e.emitToolResult(part.FunctionResponse)
		}
	}
}

// emitToolCall streams one model tool invocation as start → args → end. The
// call id prefers the runtime's FunctionCall.ID so a later FunctionResponse
// correlates. ADK v2 populates FunctionCall.ID before the event reaches this
// hook (the same id it echoes on the matching FunctionResponse), so the mint is
// dead-code defence-in-depth: it keeps the ToolCall triad internally consistent
// if a future runtime ever emits an id-less call, at the cost of a result that
// cannot be correlated back — strictly better than emitting an empty id.
func (e *aguiEmitter) emitToolCall(fc *genai.FunctionCall, parentMsgID string) {
	id := fc.ID
	if id == "" {
		id = e.nextID("tool")
	}
	e.emit(agui.NewToolCallStart(id, fc.Name, parentMsgID))
	if args, err := json.Marshal(fc.Args); err == nil {
		e.emit(agui.NewToolCallArgs(id, string(args)))
	}
	e.emit(agui.NewToolCallEnd(id))
}

// emitToolResult streams one tool response as a ToolCallResult, keyed to the
// call id the runtime recorded on the FunctionResponse.
func (e *aguiEmitter) emitToolResult(fr *genai.FunctionResponse) {
	content, err := json.Marshal(fr.Response)
	if err != nil {
		content = []byte("null")
	}
	e.emit(agui.NewToolCallResult(fr.ID, string(content)))
}

// aguiExposedWorkloads projects the bundle's agui: section into the server's
// exposed-workload list. Empty (nil) when the workload does not opt in. The
// endpoint path defaults to /agui/<name> and the description to the bundle's.
func aguiExposedWorkloads(bundle *workload.Bundle) []agui.ExposedWorkload {
	if bundle == nil || !bundle.AGUI.Expose {
		return nil
	}
	path := bundle.AGUI.EndpointPath
	if path == "" {
		path = "/agui/" + bundle.Name
	}
	desc := bundle.AGUI.Description
	if desc == "" {
		desc = bundle.Description
	}
	return []agui.ExposedWorkload{{
		WorkloadName: bundle.Name,
		EndpointPath: path,
		Description:  desc,
		InputSchema:  bundle.AGUI.InputSchema,
		Scopes:       bundle.AGUI.Auth.Scopes,
	}}
}

// aguiValidator builds the endpoint's token validator from MAST_AGUI_TOKEN.
// Unset means unauthenticated (dev only), mirroring the inject door and a2a.
// The static principal carries the union of every exposed workload's scopes so
// it can drive any of them.
func aguiValidator(logger *slog.Logger, exposed []agui.ExposedWorkload) (serverauth.TokenValidator, error) {
	token := os.Getenv("MAST_AGUI_TOKEN")
	if token == "" {
		logger.Warn("MAST_AGUI_TOKEN not set; AG-UI endpoint is unauthenticated (dev only)")
		return nil, nil
	}
	seen := map[string]bool{}
	var scopes []string
	for _, ew := range exposed {
		for _, s := range ew.Scopes {
			if !seen[s] {
				seen[s] = true
				scopes = append(scopes, s)
			}
		}
	}
	return serverauth.NewStaticBearerValidator(map[string]*serverauth.Principal{
		token: {Subject: "mast-agui-static", Scopes: scopes},
	})
}

// aguiRateLimiter builds the endpoint's rate limiter from MAST_AGUI_RATE
// (requests/second per caller×workload) and MAST_AGUI_BURST (bucket depth;
// defaults to ceil(rate), min 1). MAST_AGUI_RATE unset means no rate limiting
// (nil), mirroring the auth seam's unset-means-off default. A set but malformed
// value fails startup (fail-fast) rather than silently disabling the limit.
func aguiRateLimiter(logger *slog.Logger) (serverauth.RateLimiter, error) {
	raw := os.Getenv("MAST_AGUI_RATE")
	if raw == "" {
		return nil, nil
	}
	perSecond, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, fmt.Errorf("agui: invalid MAST_AGUI_RATE %q: %w", raw, err)
	}
	burst := int(math.Ceil(perSecond))
	if burst < 1 {
		burst = 1
	}
	if bs := os.Getenv("MAST_AGUI_BURST"); bs != "" {
		burst, err = strconv.Atoi(bs)
		if err != nil {
			return nil, fmt.Errorf("agui: invalid MAST_AGUI_BURST %q: %w", bs, err)
		}
	}
	lim, err := serverauth.NewTokenBucketLimiter(perSecond, burst)
	if err != nil {
		return nil, err
	}
	logger.Info("AG-UI rate limiting enabled", "rate_per_sec", perSecond, "burst", burst)
	return lim, nil
}

// buildAGUIServer constructs the AG-UI server for the daemon, or (nil, nil)
// when no workload opts into AG-UI exposure (the server is simply not started).
// baseCtx is the daemon's turn lifetime.
func buildAGUIServer(
	logger *slog.Logger,
	listen string,
	bundle *workload.Bundle,
	backend agui.Backend,
	metric agui.RunMetric,
	baseCtx context.Context,
) (*agui.Server, error) {
	exposed := aguiExposedWorkloads(bundle)
	if len(exposed) == 0 {
		logger.Info("AG-UI listener requested but no workload opts into AG-UI exposure (agui.expose); AG-UI disabled")
		return nil, nil
	}
	// Fail-fast on an unknown session_model rather than silently falling back to
	// per_thread at runtime (sessionModel's default): a typo'd bundle value
	// would otherwise route runs to a different session model than the operator
	// wrote, surfacing only as confused session continuity later.
	if m := bundle.AGUI.SessionModel; m != "" && m != workload.AGUISessionPerThread && m != workload.AGUISessionPerRun {
		return nil, fmt.Errorf("agui: invalid session_model %q for workload %q (want %q or %q)",
			m, bundle.Name, workload.AGUISessionPerThread, workload.AGUISessionPerRun)
	}
	validator, err := aguiValidator(logger, exposed)
	if err != nil {
		return nil, err
	}
	limiter, err := aguiRateLimiter(logger)
	if err != nil {
		return nil, err
	}
	return agui.New(agui.Config{
		Listen:      listen,
		Exposed:     exposed,
		Validator:   validator,
		Limiter:     limiter,
		Backend:     backend,
		Metric:      metric,
		Logger:      logger,
		BaseContext: baseCtx,
	})
}

// aguiListener binds the AG-UI server's listener eagerly so a bad bind address
// fails serve() at startup rather than in a background goroutine (mirrors
// buildAttach / a2aListener).
func aguiListener(listen string) (net.Listener, error) {
	return net.Listen("tcp", listen)
}
