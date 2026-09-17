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
// This wiring covers the server core (the happy-path run stream, discovery,
// auth, rate limiting) plus the HITL interrupt/resume lifecycle: a turn that
// pauses for human input finishes as RunFinished{outcome: interrupt} listing
// the open interrupts (projected from the durable session state), and a
// subsequent run carrying RunAgentInput.Resume answers them by driving a
// FunctionResponse turn through the same chokepoint — the AG-UI spelling of
// mast's durable pause/resume (docs/durable-execution-design.md), and per-key
// state deltas: a runtime state write whose key the bundle's
// agui.state_projection allowlist names is published as an RFC 6902 patch, and
// a workload that declares no allowlist publishes nothing. Reasoning is the
// second per-bundle publication surface and follows the same shape: a workload
// setting agui.emit_reasoning streams the model's thinking as a REASONING_*
// phase ahead of its answer, and one that does not emits no reasoning frame at
// all. A STEP_STARTED/STEP_FINISHED bracket names the agent that authored each
// stretch of the run, unconditionally and with no bundle key — see openStep for
// why a step is authorship here and not the design text's "turn-N". The
// ACTIVITY_* family and the agui:// federation client remain follow-on stages.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/internal/modeltext"
	"github.com/go-steer/mast/pkg/agui"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/serverauth"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// aguiBackend implements agui.Backend over the daemon's turn stack. It mirrors
// a2aBackend's seam objects but has no task registry: an AG-UI run streams
// synchronously and its terminal disposition rides the SSE stream, so there is
// no out-of-band task-state read to reconcile.
type aguiBackend struct {
	// Turn-execution seams — the same objects the inject/attach/resume/a2a
	// paths thread into runTurnPre, shared as turnDeps (#293) rather than
	// re-declared here. Embedded, so b.store / b.logger / b.obs read
	// unchanged.
	turnDeps
	bundle *workload.Bundle
}

// aguiSessionPrefix namespaces every AG-UI-derived session id. Like the
// "a2a-" task prefix it both keeps AG-UI sessions clear of other surfaces'
// namespaces (inject "incident-*", attach, autoresume) and marks a session as
// one this surface owns (see isAGUISessionID).
const aguiSessionPrefix = "agui-"

// aguiResumeFunctionName is the FunctionCall name a resume's FunctionResponse
// must carry for the runtime to route it to the parked workflow input (rather
// than silently forking a fresh turn). It matches the hardcoded name the
// operator-facing resume() path uses (see cmd/mast/main.go); the runtime's
// workflow roots filter resumes by exactly this name.
const aguiResumeFunctionName = "adk_request_input"

// sessionModel returns the workload's configured AG-UI session model,
// defaulting to per_thread (the dominant chat case: one continuing
// conversation per thread).
func (b *aguiBackend) sessionModel() string {
	if b.bundle != nil && b.bundle.AGUI.SessionModel != "" {
		return b.bundle.AGUI.SessionModel
	}
	return workload.AGUISessionPerThread
}

// aguiPublication is everything a run on this workload publishes beyond its
// message text: which runtime state keys reach the client as StateDelta
// patches, and whether the model's reasoning is streamed at all. Both are
// per-bundle opt-ins that default to publishing nothing.
//
// It is one value with one reader because it has two consumers whose
// disagreement is invisible from either side: the emitter, which decides
// what a run actually publishes, and the discovery descriptor, which tells a
// client what to expect before it runs anything (#377). Letting the second
// recompute from bundle.AGUI is the shape #364 and #375 were both about — a
// capability claim that restates the config instead of reading the thing it
// describes, and that therefore stays green while the two drift.
type aguiPublication struct {
	// stateKeys is the operator's list in the operator's order, which is
	// what the descriptor advertises: a client laying out panels should
	// get them in the order someone chose, not in map order.
	stateKeys []string
	// stateSet is the same list as a lookup, because a state write
	// arrives on the hot event path and the emitter consults it per key.
	// Nil when the bundle declares no projection, which is the default
	// and means no StateDelta is ever emitted.
	stateSet map[string]bool
	// reasoning is agui.emit_reasoning. False for a nil bundle and by
	// default, and false means no REASONING_* frame of any kind.
	reasoning bool
}

// publication reads the bundle's two publication switches. The only place in
// the daemon that reads them; see aguiPublication for why that matters.
func (b *aguiBackend) publication() aguiPublication {
	if b.bundle == nil {
		return aguiPublication{}
	}
	pub := aguiPublication{reasoning: b.bundle.AGUI.EmitReasoning}
	if len(b.bundle.AGUI.StateProjection) == 0 {
		return pub
	}
	pub.stateKeys = b.bundle.AGUI.StateProjection
	pub.stateSet = make(map[string]bool, len(pub.stateKeys))
	for _, k := range pub.stateKeys {
		pub.stateSet[k] = true
	}
	return pub
}

// AGUICapabilities implements agui.CapabilityReporter: it tells a discovery
// client which optional frame families a run on this workload can contain.
//
// Every field is derived from the same aguiPublication the emitter is built
// from, and StateDelta specifically reads len(stateSet) — the map the
// emitter consults per state write — rather than the bundle list, so the
// advertised bit is a reading of the emitter's own input and not a second
// opinion about it.
//
// A name this backend does not serve gets the zero value rather than this
// workload's answer. The daemon runs one bundle today, so the mismatch
// cannot happen; answering anyway for whatever name is asked is how it would
// silently start lying the day that changes.
func (b *aguiBackend) AGUICapabilities(workloadName string) agui.AgentCapabilities {
	if b.bundle == nil || workloadName != b.bundle.Name {
		return agui.AgentCapabilities{}
	}
	pub := b.publication()
	return agui.AgentCapabilities{
		StateDelta: len(pub.stateSet) > 0,
		StateKeys:  pub.stateKeys,
		Reasoning:  pub.reasoning,
	}
}

// sessionIDFor derives the mast session id for a run from the client-supplied
// threadId/runId, always namespaced under the AG-UI prefix. The client never
// supplies a raw session id — the daemon derives it — so a caller cannot drive
// a turn into another surface's session by presenting its id (structural
// separation, stronger than a runtime check). When the correlation id the
// session model keys on is absent, a fresh owned session is minted rather than
// colliding every id-less caller onto one shared session.
//
// The authenticated caller is part of the derivation, which is what makes a
// thread belong to whoever opened it (#382). Scope-checking answers "may this
// caller run this workload"; it does not answer "is this conversation yours",
// and without the caller in the id a second principal holding the same scope
// read and continued the first's thread by naming its threadId. Folding the
// identity in rather than recording an owner and comparing is the same choice
// the paragraph above makes for surface separation: a foreign caller does not
// get refused, they address a different session and never reach this one.
//
// The caller is hashed to a fixed-width tag, not interpolated raw: a
// variable-width identity next to an attacker-chosen threadId is a delimiter
// ambiguity, and two callers must never be able to construct the same id.
func (b *aguiBackend) sessionIDFor(threadID, runID string, p *serverauth.Principal) string {
	owner := callerTag(p)
	switch b.sessionModel() {
	case workload.AGUISessionPerRun:
		if runID != "" {
			return aguiSessionPrefix + "run-" + owner + runID
		}
	default:
		if threadID != "" {
			return aguiSessionPrefix + "thread-" + owner + threadID
		}
	}
	return mintID(aguiSessionPrefix)
}

// callerTag renders an authenticated caller as a fixed-width session-id
// segment, or the empty string when the endpoint has no validator.
//
// Empty for an unauthenticated endpoint is deliberate and mirrors attach's
// enforceACL posture: with no validator there is no subject, so there is no
// identity to own anything, and a single-operator deployment keeps exactly the
// session ids it had. Tenant is hashed alongside subject because two tenants
// may legitimately issue the same subject string.
func callerTag(p *serverauth.Principal) string {
	if p == nil || (p.Subject == "" && p.Tenant == "") {
		return ""
	}
	sum := sha256.Sum256([]byte(p.Tenant + "\x00" + p.Subject))
	return hex.EncodeToString(sum[:6]) + "-"
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
// returned RunResult. A run carrying RunAgentInput.Resume answers the session's
// open interrupts with FunctionResponses instead of a user message, resuming a
// parked turn. A pre-turn refusal (draining; a crafted id that escaped the owned
// namespace; a resume against a session not awaiting input) returns before any
// emit so the server reports a clean HTTP status rather than an orphaned stream.
func (b *aguiBackend) RunAgent(ctx context.Context, in agui.RunInput, emit func(any)) (agui.RunResult, error) {
	// Drain gate: refuse new work once shutdown has begun, BEFORE any emit
	// (mirrors the inject handler and a2a). ErrUnavailable → HTTP 503.
	if b.tracker.isDraining() {
		return agui.RunResult{}, fmt.Errorf("agui: server draining, not accepting new runs: %w", agui.ErrUnavailable)
	}

	// A resume arrives as a new run (new RunID) but must reach the session the
	// parent run parked on. Under the run-keyed model (per_run) that session is
	// keyed on the ORIGINAL run's id, so a spec-compliant resume carries
	// parentRunId and we key on it; without it a per_run resume cannot locate the
	// parked session and correctly 409s below. Under the thread-keyed model
	// (per_thread, the default) the session ignores RunID entirely, so this is a
	// no-op there — the same threadId already reaches the parked session.
	runID := in.RunID
	if len(in.Resume) > 0 && in.ParentRunID != "" {
		runID = in.ParentRunID
	}
	sessionID := b.sessionIDFor(in.ThreadID, runID, in.Principal)
	if !isAGUISessionID(sessionID) {
		// A crafted threadId/runId collided with the reserved ops-row
		// namespace; refuse before any emit rather than drive a turn into a
		// reserved row (which would corrupt its marker storage).
		return agui.RunResult{}, fmt.Errorf("agui: derived session id %q is not addressable", sessionID)
	}

	// Build the turn message. A resume (RunAgentInput.Resume present) answers the
	// session's open interrupts with FunctionResponses; a fresh run carries the
	// user text. The resume's pre-validation runs BEFORE any emit so a resume
	// against a session that is not awaiting input is refused as a clean HTTP 409
	// (ErrNotResumable) rather than an orphaned stream.
	label := "agui:run"
	msg := genai.NewContentFromText(in.Text, genai.RoleUser)
	if len(in.Resume) > 0 {
		resumeMsg, rerr := b.buildResumeMessage(ctx, sessionID, in.Resume)
		if rerr != nil {
			return agui.RunResult{}, rerr
		}
		msg, label = resumeMsg, "agui:resume"
	}

	// Time the executed run for the duration histogram; only runs that pass
	// the pre-turn refusals above are measured, so the histogram reflects real
	// turn wallclock rather than fast rejections.
	start := time.Now()
	defer func() { b.obs.AGUIRunDuration(b.workloadName, time.Since(start).Seconds()) }()

	// Opening frames (mirrors A2A's initial Task snapshot as its first emit):
	// RunStarted, then a StateSnapshot echoing the client's input state, or an
	// empty object when absent. Interior StateDelta patches follow from the
	// projection allowlist, if the workload declares one.
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

	pub := b.publication()
	em := &aguiEmitter{
		emit:       emit,
		projection: pub.stateSet,
		reasoning:  pub.reasoning,
		logger:     b.logger,
	}
	err := runTurnPre(ctx, b.turnDeps, sessionID, msg, label, nil, em.onEvent)

	// Close the open author bracket before the server writes the terminal
	// frame, whatever the disposition: a dangling STEP_STARTED outlives the run
	// in a client that tracks it.
	em.closeStep()

	return b.classifyRun(ctx, sessionID, em, err)
}

// buildResumeMessage turns a run's Resume entries into the FunctionResponse turn
// that unparks the session, mirroring the operator-facing resume() path
// (cmd/mast/main.go): each answered interrupt becomes a FunctionResponse named
// aguiResumeFunctionName, carrying the interrupt id and a {"response": <answer>}
// map. It reads the durable pause projection (the ground truth for which
// interrupts are open) and answers only entries that name a genuinely-open
// interrupt — an entry naming no open interrupt is dropped, since replaying it as
// a FunctionResponse would either be ignored by the runtime or fork a fresh turn.
// When no entry matches an open interrupt (a resume against a session not
// awaiting input, or answering already-closed interrupts) it returns
// ErrNotResumable so the server refuses with a clean HTTP 409 before any emit.
func (b *aguiBackend) buildResumeMessage(ctx context.Context, sessionID string, entries []agui.ResumeEntry) (*genai.Content, error) {
	d, err := b.store.Get(ctx, "", sessionID)
	if err != nil {
		return nil, fmt.Errorf("agui: cannot resume %q: %w", sessionID, agui.ErrNotResumable)
	}
	open := map[string]bool{}
	for _, id := range d.PendingInterruptIDs {
		open[id] = true
	}
	var parts []*genai.Part
	for _, e := range entries {
		if e.InterruptID == "" || !open[e.InterruptID] {
			continue
		}
		part := genai.NewPartFromFunctionResponse(aguiResumeFunctionName, map[string]any{
			"response": resumeAnswer(e),
		})
		part.FunctionResponse.ID = e.InterruptID
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("agui: no open interrupt matched the resume for %q: %w", sessionID, agui.ErrNotResumable)
	}
	return &genai.Content{Role: genai.RoleUser, Parts: parts}, nil
}

// resumeAnswer decodes a resume entry's operator-supplied payload into the value
// forwarded to the parked tool. The payload is forwarded verbatim; a resolved
// entry with no payload forwards an empty object and a cancelled entry with no
// payload forwards a minimal {"status":"cancelled"} disposition so the tool can
// distinguish a cancellation from a resolution with an empty answer.
func resumeAnswer(e agui.ResumeEntry) any {
	if len(e.Payload) > 0 {
		var v any
		if err := json.Unmarshal(e.Payload, &v); err == nil {
			return v
		}
	}
	if e.Status == agui.ResumeStatusCancelled {
		return map[string]any{"status": string(agui.ResumeStatusCancelled)}
	}
	return map[string]any{}
}

// interruptsFromDetail projects a session's durable pending inputs into the
// AG-UI interrupt list carried on RunFinished{outcome: interrupt}. Each pending
// input contributes its id and operator-facing message; its response schema (if
// any) is marshaled to raw JSON for the client to render an input form.
func interruptsFromDetail(d *transcript.Detail) []agui.Interrupt {
	out := make([]agui.Interrupt, 0, len(d.Pending))
	for _, p := range d.Pending {
		it := agui.Interrupt{ID: p.InterruptID, Message: p.Message}
		if p.ResponseSchema != nil {
			if raw, err := json.Marshal(p.ResponseSchema); err == nil {
				it.ResponseSchema = raw
			}
		}
		out = append(out, it)
	}
	return out
}

// pendingInterrupts reads the session's durable state and, when it is paused with
// open interrupts, projects them into the AG-UI interrupt list. ok is false when
// the read failed or the session is not paused on input, so a clean-finishing
// turn is reported as a plain success rather than a spurious interrupt.
func (b *aguiBackend) pendingInterrupts(ctx context.Context, sessionID string) ([]agui.Interrupt, bool) {
	d, err := b.store.Get(ctx, "", sessionID)
	if err != nil || d.State != transcript.StatePaused || len(d.Pending) == 0 {
		return nil, false
	}
	return interruptsFromDetail(d), true
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
		// A HITL pause finishes the turn with err==nil (no sentinel); the durable
		// pause projection is the ground truth for whether the turn parked and on
		// what. When it did, report the open interrupts so the server emits
		// RunFinished{outcome: interrupt} listing them; otherwise the turn is a
		// plain success.
		if its, ok := b.pendingInterrupts(readCtx, sessionID); ok {
			return agui.RunResult{Text: em.lastText, Interrupted: true, Interrupts: its}, nil
		}
		if em.interrupted {
			// In-turn interrupt signal but no durable pause projection — a transient
			// store read failure, or a pause whose marker did not land. Should not
			// happen (the pause writes its marker synchronously). Do NOT advertise a
			// resume-less interrupt (an interrupt outcome with an empty interrupts
			// list the client can never answer) and do NOT fabricate a success:
			// close the stream with an honest internal error instead.
			return agui.RunResult{}, fmt.Errorf("agui: interrupt signaled but no durable pause projection for session %q", sessionID)
		}
		return agui.RunResult{Text: em.lastText}, nil
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
		return agui.RunResult{Interrupted: true, Interrupts: interruptsFromDetail(d)}, true
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

	// projection is the workload's agui.state_projection allowlist as a set.
	// Nil means the workload publishes no state, so onEvent never looks at a
	// state delta at all.
	projection map[string]bool

	// reasoning is the workload's agui.emit_reasoning opt-in. False — the
	// default — means onEvent never reads a thinking part at all.
	reasoning bool

	// logger records a state value that will not marshal. Optional: the
	// emitter is constructed in tests without one.
	logger *slog.Logger

	// step is the name of the open STEP_STARTED bracket — the agent that
	// authored the most recent model event — or "" when none is open.
	step string

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
// A model event's thinking parts become a REASONING_* bracket ahead of the
// answer, but only for a workload that opted in (see emitReasoning).
func (e *aguiEmitter) onEvent(ev *session.Event) {
	if ev == nil {
		return
	}
	if ev.RequestedInput != nil || len(ev.LongRunningToolIDs) > 0 {
		e.interrupted = true
	}
	// State writes ride on the event's Actions, not its Content, so this runs
	// before the Content nil-check: pkg/graph stashes a node result on an event
	// that carries no content at all.
	e.emitStateDelta(ev.Actions.StateDelta)
	if ev.Content == nil {
		return
	}
	model := ev.Content.Role == genai.RoleModel

	// The step bracket opens before this event's own frames, so the reasoning
	// and answer below land inside the step that produced them.
	if model {
		e.openStep(ev.Author)
	}

	// Reasoning before the answer, because that is the order the model
	// produced them in (a thinking part precedes the text part in the same
	// content) and a client renders the stream in arrival order.
	if model {
		e.emitReasoning(ev.Content.Parts)
	}

	// Assistant text: one whole message per model event. User/tool echoes on
	// the stream are not re-emitted as assistant text.
	var msgID string
	if model {
		var sb strings.Builder
		for _, part := range ev.Content.Parts {
			if text, ok := modeltext.Text(part); ok {
				sb.WriteString(text)
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

// openStep opens a STEP_STARTED bracket for author, closing the previous one
// first, and does nothing if that author's step is already open. Together with
// closeStep it answers the question the AG-UI step family asks and the protocol
// deliberately does not: what is a step in mast (#98, resolving
// docs/ag-ui-design.md's activity-events row).
//
// **A step is the stretch of a run authored by one agent, named after it.**
// The design text proposed `stepName: "turn-N"`, and that is degenerate here:
// an AG-UI run drives exactly one turn through runTurnPre, so every run would
// report turn-1 and the frame would carry no information. Authorship does
// carry information — it is where a coordinator hands off to a specialist, and
// where a graph node's agent takes over — which is the "workflow shape in the
// UI" the row was actually asking for.
//
// Three properties are worth stating.
//
// **It reads session.Event.Author, and only on a model-authored event.** Author
// is the attribution seam the rest of mast already trusts: pkg/budget buckets
// spend by it, pkg/effects classifies by it, and pkg/specialists records that
// it carries the agent's name on every dispatch shape mast builds (Branch does
// not — it is empty in the coordinator/sub-agent-tool shape). Restricting the
// read to model events means the author is an agent by construction, so no
// roster set has to be threaded here to keep a user echo or a runtime-
// synthesized event from naming a step after something that is not one.
//
// **It is unconditional — no bundle key.** Unlike the state projection and
// reasoning, this publishes nothing a permitted client could not already see:
// a handoff reaches the same stream as a transfer_to_agent / invoke_specialist
// ToolCallStart naming the very same agent. That is #377's rule for the public
// descriptor applied to a frame family, and with nothing to switch off, a key
// would only add a bundle field that can be set wrong (and, since #302, a
// spelling that is fatal at load).
//
// **The brackets are flat, and under a parallel fan-out one name can bracket
// more than once.** Events arrive serialized on one stream, so this reports the
// order they arrived in rather than a nesting mast cannot observe; a fan-out
// whose workers interleave produces alternating brackets. Reporting arrival
// order is honest, and collapsing it would invent a structure.
func (e *aguiEmitter) openStep(author string) {
	if author == "" || author == e.step {
		return
	}
	e.closeStep()
	e.emit(agui.NewStepStarted(author))
	e.step = author
}

// closeStep finishes the open bracket, if any. RunAgent calls it once the turn
// returns — for every disposition, including an abort and a HITL pause, because
// a client tracking an open step has no other way to learn the run ended and a
// dangling STEP_STARTED is worse than a step that ends early.
func (e *aguiEmitter) closeStep() {
	if e.step == "" {
		return
	}
	e.emit(agui.NewStepFinished(e.step))
	e.step = ""
}

// emitReasoning publishes one model event's thinking parts as a REASONING_*
// phase, for a workload whose bundle sets agui.emit_reasoning (#98, resolving
// docs/ag-ui-design.md OQ 5). It is the second publication surface governed
// per bundle, and it follows the state-projection precedent deliberately:
// off by default, nothing at all when off, and said out loud at startup.
//
// Four properties are the substance of the opt-in.
//
// It is a **deliberate read, not an unfiltered one**. The text comes from
// internal/modeltext.Thought — the named counterpart to the Text predicate
// every other surface reads through (#370) — so publishing reasoning is a
// call to a function whose name says so, and an audit of what can reach a
// browser is a grep rather than a review of every part.Text in the tree.
//
// When off it emits **nothing**, not an empty bracket and not a marker, so a
// client cannot learn that the model reasoned. That is the same filter-not-
// redaction rule the state projection follows, and for the same reason: the
// existence of a thought can itself be the disclosure.
//
// A thinking block with **no prose is not a phase**. Under the request mast
// sends today claude-opus-5 returns a signed block with an empty body, so the
// common case for an opted-in workload is that this emits nothing — correctly.
// A bracket around no content would tell a client a thought is being withheld,
// and the signature that block does carry has no AG-UI frame at all.
//
// The parts of one event are **concatenated into one reasoning message**,
// mirroring the answer path directly above: mast runs StreamingModeNone, so
// an event is a whole message, and splitting one model turn's thinking across
// several messages would invent a structure the provider did not send.
func (e *aguiEmitter) emitReasoning(parts []*genai.Part) {
	if !e.reasoning {
		return
	}
	var sb strings.Builder
	for _, p := range parts {
		if thought, ok := modeltext.Thought(p); ok {
			sb.WriteString(thought)
		}
	}
	if sb.Len() == 0 {
		return
	}
	id := e.nextID("reasoning")
	e.emit(agui.NewReasoningStart())
	e.emit(agui.NewReasoningMessageStart(id))
	e.emit(agui.NewReasoningMessageContent(id, sb.String()))
	e.emit(agui.NewReasoningMessageEnd(id))
	e.emit(agui.NewReasoningEnd())
}

// emitStateDelta publishes the allowlisted subset of one event's runtime state
// writes as an RFC 6902 JSON Patch (#98, resolving docs/ag-ui-design.md OQ 7).
//
// Three properties are deliberate. It is a **filter, not a redaction**: a key
// the projection does not name produces no op, rather than an op with a
// scrubbed value, so a client cannot learn that a key it may not see changed.
// Ops are emitted in **sorted key order**, because Go's map iteration is
// randomized and a patch stream a client diffs across runs should not reorder
// for no reason. And a value that will not marshal is **dropped with a warning
// rather than emitted as null** — null is a legitimate state value, so a
// marshal failure rendered as one would publish a change that did not happen.
func (e *aguiEmitter) emitStateDelta(delta map[string]any) {
	if len(e.projection) == 0 || len(delta) == 0 {
		return
	}
	keys := make([]string, 0, len(delta))
	for k := range delta {
		if e.projection[k] {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	ops := make([]agui.PatchOp, 0, len(keys))
	for _, k := range keys {
		raw, err := json.Marshal(delta[k])
		if err != nil {
			if e.logger != nil {
				e.logger.Warn("AG-UI state projection skipped a key whose value will not marshal",
					"key", k, "error", err)
			}
			continue
		}
		ops = append(ops, agui.PatchOp{Op: "add", Path: agui.StatePointer(k), Value: raw})
	}
	if len(ops) == 0 {
		return
	}
	e.emit(agui.NewStateDelta(ops))
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

// checkStateProjection refuses an agui.state_projection list that cannot mean
// what its author meant, at startup rather than at the first state write.
//
// Two shapes are refused and one deliberately is not. An **empty entry** is
// refused because it can only come from a stray "-" or a trailing comma, and
// it would silently match no key forever — the failure mode #290's decorative
// RBAC binding had. A **duplicate** is refused because a list a reader counts
// to answer "what does this publish?" must not double-count, and because the
// duplicate is usually a half-finished edit. A key that names state the
// workload never writes is **not** refused: which keys a roster produces
// depends on the dispatch shape and on tools resolved at runtime, so a
// startup check could only guess, and guessing wrong would refuse a correct
// bundle.
func checkStateProjection(bundle *workload.Bundle) error {
	seen := map[string]bool{}
	for i, k := range bundle.AGUI.StateProjection {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("agui: state_projection[%d] for workload %q is empty: remove the entry or name a state key", i, bundle.Name)
		}
		if seen[k] {
			return fmt.Errorf("agui: state_projection for workload %q names %q twice", bundle.Name, k)
		}
		seen[k] = true
	}
	return nil
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
	if err := checkStateProjection(bundle); err != nil {
		return nil, err
	}
	if n := len(bundle.AGUI.StateProjection); n > 0 {
		// Said out loud at startup for the same reason the builtin-tools summary
		// is (#324): a publication allowlist is a security setting, and an
		// operator should be able to read what this daemon publishes off the log
		// rather than off the bundle they think is mounted.
		logger.Info("AG-UI state projection enabled",
			"workload", bundle.Name, "keys", bundle.AGUI.StateProjection)
	}
	if bundle.AGUI.EmitReasoning {
		// Warn, where the state-projection line above is Info, and the
		// difference is deliberate rather than an inconsistency. A projection
		// publishes keys the operator chose one at a time; emit_reasoning
		// publishes whatever the model happened to think, which is the one
		// text on this surface nobody wrote for an audience — including,
		// when a turn is injected, the injection's own working. It is a
		// legitimate setting and not a misconfiguration, but an operator
		// scanning a running daemon should not have to already suspect it.
		logger.Warn("AG-UI reasoning publication enabled: this workload streams the model's thinking to its AG-UI clients",
			"workload", bundle.Name)
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
