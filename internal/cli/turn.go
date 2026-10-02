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

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	mastagent "github.com/go-steer/mast/internal/agent"
	"github.com/go-steer/mast/internal/auth"
	"github.com/go-steer/mast/internal/effects"
	"github.com/go-steer/mast/internal/envelope"
	"github.com/go-steer/mast/internal/inject"
	"github.com/go-steer/mast/internal/observability"
	"github.com/go-steer/mast/internal/transcript"
	"github.com/go-steer/mast/internal/watchdog"
	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/workload"
)

// reservedPayloadErr rejects payloads whose derived session ID uses
// the reserved ops-row suffix (#61): the UID is untrusted and mints
// the session ID, so it is a session-ID surface like any other — a
// reserved ID would create a session the marker machinery corrupts
// and the operator surface hides. Wraps inject.ErrBadPayload so the
// server answers 400, not 500 (emitters must not retry it).
func reservedPayloadErr(p envelope.InjectPayload) error {
	if sid := sessionIDFor(p); transcript.IsReservedSessionID(sid) {
		return fmt.Errorf("payload uid %q derives reserved session ID %q: %w", p.UID, sid, inject.ErrBadPayload)
	}
	return nil
}

// sessionIDFor derives a per-incident session ID from the payload so
// each incident's history, pauses, and resumes are isolated (spike-2
// change; spike 1 funneled every inject into one shared session).
func sessionIDFor(p envelope.InjectPayload) string {
	if p.UID != "" {
		return "incident-" + p.UID
	}
	return defaultSessionID
}

// prependFeedback drains the session's pending watchdog observations
// onto the front of the turn's message. Returns msg untouched when
// nothing is pending — which is every turn below --watchdog=feedback,
// and most turns above it.
//
// A fresh Content rather than an edit in place: msg belongs to its
// caller (the inject handler, the scheduler, an A2A task), and growing
// their slice would leak the block into a retry of the same message.
func prependFeedback(fb *watchdog.Feedback, msg *genai.Content) *genai.Content {
	if msg == nil {
		// Nothing to prepend to. Leave the queue alone rather than
		// draining an observation into a message that will never be
		// sent.
		return nil
	}
	block := watchdog.FormatFeedback(fb.Drain())
	if block == "" {
		return msg
	}
	parts := make([]*genai.Part, 0, len(msg.Parts)+1)
	parts = append(parts, genai.NewPartFromText(block+"\n\n---\n"))
	return &genai.Content{Role: msg.Role, Parts: append(parts, msg.Parts...)}
}

// toolPolicies converts the bundle's tool_catalog per-tool overrides
// into the shape internal/effects consumes (nil bundle = no overrides).
func toolPolicies(bundle *workload.Bundle) []effects.ToolPolicy {
	if bundle == nil {
		return nil
	}
	out := make([]effects.ToolPolicy, 0, len(bundle.ToolCatalog.Tools))
	for _, p := range bundle.ToolCatalog.Tools {
		out = append(out, effects.ToolPolicy{Name: p.Name, Mutating: p.Mutating})
	}
	return out
}

// turnDeps is the set of objects every turn runs against, fixed for the
// daemon's lifetime and identical on all six surfaces that start one
// (inject, resume, attach, A2A, AG-UI, the scheduled trigger and
// auto-resume). It exists because that list was threaded positionally
// through runTurnPre from all of them, and because two of those
// surfaces had already grown a private copy of it — a2aBackend and
// aguiBackend each carried these nine fields with a comment saying they
// were "the same objects the inject/attach/resume paths thread into
// runTurnPre". Both now embed this instead, so adding a dependency is
// one field rather than six call sites and two duplicated structs
// (#293).
//
// None of these are per-turn: the session id, the message and the label
// stay positional below, because they are what distinguishes one turn
// from the next and burying them in a struct would hide the arguments a
// reader actually needs to see at a call site.
type turnDeps struct {
	r            *runner.Runner
	logger       *slog.Logger
	store        *transcript.Store
	meters       *meterPool
	wds          *watchdogPool
	obs          *observability.Registry
	tracker      *turnTracker
	turnLocks    *sessionTurnLocks
	workloadName string
}

func dispatch(ctx context.Context, d turnDeps, bundle *workload.Bundle, p envelope.InjectPayload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal inject payload: %w", err)
	}
	// Workload wallclock ceiling: bound the whole turn.
	if bundle != nil && bundle.Budget.MaxWallclockSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(bundle.Budget.MaxWallclockSeconds)*time.Second)
		defer cancel()
	}
	msg := genai.NewContentFromText(fmt.Sprintf("INJECT %s", string(body)), genai.RoleUser)
	return runTurn(ctx, d, sessionIDFor(p), msg, "inject:"+p.Reason)
}

// resume feeds an operator's approval verdict back into a paused
// session. The runner treats a user turn carrying a FunctionResponse
// whose ID matches a pending InterruptID as a resume (see adk/v2
// runner buildResumeResponses).
func resume(ctx context.Context, d turnDeps, bundle *workload.Bundle, req inject.ResumeRequest, preTurn func(context.Context) error) error {
	// Same wallclock ceiling as the inject and attach paths — resume
	// turns are not budget-exempt either (#47).
	if bundle != nil && bundle.Budget.MaxWallclockSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(bundle.Budget.MaxWallclockSeconds)*time.Second)
		defer cancel()
	}
	msg, err := resumeMessage(ctx, d.store, req)
	if err != nil {
		return err
	}
	d.obs.HITLResume(d.workloadName)
	return runTurnPre(ctx, d, req.SessionID, msg, "resume:"+req.InterruptID, preTurn, nil)
}

// resumeMessage builds the user turn that answers a pending interrupt.
//
// Two pause primitives share one endpoint, and they do NOT share a wire
// shape. A RequestInput park is answered under the name
// adk_request_input, which workflowagent roots (planner, graph) filter
// resume responses by before matching the ID — any other name silently
// forks a fresh turn instead of resuming (v0.2 pause/abort design, fact
// 4). A write-gate park is an ADK tool confirmation and must be
// answered under adk_request_confirmation with {confirmed, payload},
// which is what RequestConfirmationRequestProcessor looks for before it
// re-dispatches the original call. Sending either shape to the other
// kind of pause leaves the session parked with an operator convinced
// they answered it.
//
// The kind is read from the durable log rather than declared by the
// client: the client is answering a question mast asked, and mast is
// the one who knows what it asked.
func resumeMessage(ctx context.Context, store *transcript.Store, req inject.ResumeRequest) (*genai.Content, error) {
	var part *genai.Part
	if isConfirmationPark(ctx, store, req) {
		v, err := verdictFor(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", inject.ErrBadPayload, err)
		}
		part = genai.NewPartFromFunctionResponse(toolconfirmation.FunctionCallName, approval.ConfirmationResponse(v))
	} else {
		part = genai.NewPartFromFunctionResponse("adk_request_input", map[string]any{
			"response": req.Response,
		})
	}
	part.FunctionResponse.ID = req.InterruptID
	return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{part}}, nil
}

// isConfirmationPark reports whether the named interrupt is a parked
// mutating tool call. A session the store cannot read, or an interrupt
// ID it does not know, is not an error here: the runTurnPre chokepoint
// and ADK's own resume matching are the authoritative checks, and this
// function's only job is picking the wire shape.
func isConfirmationPark(ctx context.Context, store *transcript.Store, req inject.ResumeRequest) bool {
	d, err := store.Get(ctx, "", req.SessionID)
	if err != nil {
		return false
	}
	for _, p := range d.Pending {
		if p.InterruptID == req.InterruptID {
			return p.ToolName == toolconfirmation.FunctionCallName
		}
	}
	return false
}

// verdictFor decodes the operator's answer and stamps it with the
// authenticated approver.
//
// The approver is taken from the request context, never from the
// payload: "who approved this" is the audit question the write gate
// exists to answer, and a self-asserted answer is not an answer. A
// client that sends one has it overwritten silently — there is nothing
// for the operator to fix, and refusing the resume over a field the
// client had no business setting would strand a real approval.
func verdictFor(ctx context.Context, req inject.ResumeRequest) (approval.Verdict, error) {
	raw, err := json.Marshal(req.Response)
	if err != nil {
		return approval.Verdict{}, fmt.Errorf("re-marshalling resume response: %w", err)
	}
	var v approval.Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return approval.Verdict{}, fmt.Errorf("resume response is not a verdict record: %w", err)
	}
	switch v.Verdict {
	case approval.OutcomeApprove, approval.OutcomeReject, approval.OutcomeEdit:
	default:
		return approval.Verdict{}, fmt.Errorf("unknown verdict %q (want approve, reject, or edit)", v.Verdict)
	}
	v.Approver = approverFromContext(ctx)
	return v, nil
}

// approverFromContext names the authenticated caller behind a resume.
// The empty string is impossible on the /resume path (internal/inject always
// attributes at least the shared credential) but reachable from the
// in-process callers — the timed-pause scheduler, boot-time auto-resume
// — where naming the mechanism is the truthful answer.
func approverFromContext(ctx context.Context) string {
	return auth.Attribution(ctx, "mast:internal")
}

// consumeIfAnswered consumes a plane-A pause token iff the resume
// FunctionResponse durably landed — pinned by the design (gate finding
// M5): consumption keys on the append, not on turn completion. A
// resume turn that failed BEFORE the append leaves the interrupt
// pending and the token live (retry works); one that failed after has
// still legitimately ended the pause.
func consumeIfAnswered(ctx context.Context, store *transcript.Store, logger *slog.Logger, rec *transcript.PauseRecord, by string) {
	d, err := store.Get(ctx, "", rec.SessionID)
	if err != nil {
		return
	}
	for _, id := range d.PendingInterruptIDs {
		if id == rec.InterruptID {
			return // still pending: the resume never appended
		}
	}
	// ConsumeScheduled, not ConsumeToken: this runs AFTER the resume has
	// durably appended (M5 — consumption keys on the append, not on
	// gating). The pause has legitimately ended; the operator-facing
	// token TTL must not veto the bookkeeping consume and strand an
	// answered interrupt with a live-looking record.
	if _, err := store.ConsumeScheduled(ctx, rec.Token, by); err != nil &&
		!errors.Is(err, transcript.ErrAlreadyResumed) && !errors.Is(err, transcript.ErrTokenNotFound) {
		logger.Error("failed to consume resume token after answered interrupt",
			"session", rec.SessionID, "error", err.Error())
	}
}

func runTurn(ctx context.Context, d turnDeps, sessionID string, msg *genai.Content, label string) error {
	return runTurnPre(ctx, d, sessionID, msg, label, nil, nil)
}

// runTurnPre is runTurn with an optional hook that runs under the
// session's turn lock, before the turn starts. The resume path uses it
// to write the effects-ack watermark: under the lock, no in-flight
// turn can still be persisting mutating intents the watermark would
// silently cover; before the turn, the outbox's turn-start scan sees
// the watermark durably.
//
// It is also the v0.2 pause/abort CHOKEPOINT: every turn kind — inject,
// attach, resume, timer — passes here, so aborted sessions (terminal;
// ADK has no engine state to delegate to) and gate-paused sessions
// refuse here, under the turn lock. The cancel handle registers BEFORE
// the marker check (the register-before-check half of the abort/hard-
// pause handshake): a sweep after a marker write either finds this
// turn registered and cancels it, or this check sees the marker first.
// onEvent, when non-nil, is invoked for each runner event after it is
// logged and metered — the A2A message/send path uses it to capture the
// final assistant answer and any HITL-pause signal for its reply.
func runTurnPre(ctx context.Context, d turnDeps, sessionID string, msg *genai.Content, label string, preTurn func(context.Context) error, onEvent func(*session.Event)) (err error) {
	// The turn's own span, opened before anything can refuse the turn
	// so a refusal is traceable too, and before the lock so the queue
	// wait is inside it. Everything ADK emits below — invoke_agent,
	// generate_content, execute_tool — parents off the ctx returned
	// here; on the paths with no HTTP request behind them (scheduled
	// fire, auto-resume) that is the difference between a trace and a
	// pile of roots. See turnspan.go.
	ctx, ts := startTurnSpan(ctx, d.obs, d.workloadName, sessionID, label)
	defer func() { ts.end(err) }()

	// One turn per session (#62): ADK's stale-session check makes a
	// second concurrent runner turn on the same row fatal to one of
	// them, so same-session turns queue here. The wait genuinely
	// honors ctx (channel semaphore) — bounded by the wallclock
	// budget, the request lifetime, and drain-expiry cancellation.
	unlock, err := d.turnLocks.lock(ctx, sessionID)
	if err != nil {
		if d.tracker.isDraining() {
			// The wait was cut by drain-expiry cancellation: this is
			// the daemon refusing work, not a dispatch failure — 503,
			// same contract as the drain gate (#65).
			return fmt.Errorf("%w (queued turn cancelled: %v)", inject.ErrUnavailable, err)
		}
		return err
	}
	defer unlock()
	ts.locked()

	// The cancel handle doubles as the budget-trip cancel below and the
	// abort / hard-pause sweep target.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.tracker.registerCancel(sessionID, cancel)
	defer d.tracker.unregisterCancel(sessionID)

	// …and it is also how the write gate ends a turn whose model will
	// not accept a refusal (#449). Installed on the context, not looked
	// up by session, so it reaches a planner-dispatched specialist on
	// its own sub-runner; read back next to the watchdog's Preflight
	// below, because a cancelled run reports "context canceled"
	// whichever of the two pulled the trigger.
	refused := &refusalStop{}
	refused.arm(cancel)
	ctx = approval.WithTurnStop(ctx, refused)

	if err := admitTurn(ctx, d, sessionID); err != nil {
		return err
	}

	if preTurn != nil {
		if err := preTurn(ctx); err != nil {
			return err
		}
	}

	// Shutdown bookkeeping brackets the whole turn (see turnTracker).
	d.tracker.begin(sessionID)
	defer d.tracker.end(sessionID)

	// Budget enforcement point: the meter folds UsageMetadata from
	// each streamed event; crossing a ceiling cancels the run context,
	// aborting any in-flight model/tool work.
	meter := d.meters.meter(sessionID)

	// The pre-call half of the same ceiling (W10.2). Observe below is
	// the durable ledger and stays exactly as it is — a call that
	// crossed cost what it cost, and #175 needs that written down. This
	// puts the meter in front of the call as well, so a workload one
	// call from its cap does not have to spend that call to discover it.
	// The gate rides the context on purpose: it is how the check reaches
	// a planner-dispatched specialist, whose sub-runner has its own
	// session id and would miss a meter looked up by session.
	ctx = mastagent.WithCallGate(ctx, meter)

	// What the meter had already refused and spent before this turn, so
	// what the turn itself did is a delta (see turnBudget).
	tb := newTurnBudget(meter)
	defer tb.finish(d, ts, label, sessionID)

	// Feedback mode (--watchdog=feedback and up): whatever the session's
	// previous turns tripped goes in front of this turn's prompt. Every
	// posture below this one routes the observation to an operator; on
	// an unattended workload that operator is a log nobody is tailing,
	// and the model about to repeat the call is the only party that can
	// decide not to.
	msg = prependFeedback(d.wds.feedback(sessionID), msg)

	// Watchdog tap (internal/watchdog): per-session accumulation across
	// turns, per-turn dedup of aggregator re-emissions (core-agent
	// #363).
	enf := d.wds.enforcer(sessionID)
	fb := d.wds.feedback(sessionID)
	onAlert := watchdogAlertHandler(d, label, sessionID, enf, fb, cancel)

	events := 0
	for event, err := range watchdog.Tap(d.r.Run(ctx, defaultUserID, sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}), d.wds.watchdog(sessionID), onAlert) {
		if err != nil {
			// A halt cancels the run context, so the runner's own
			// error here is "context canceled" — reporting that would
			// bury the reason under a symptom.
			if terr := enf.Preflight(); terr != nil {
				ts.complete(observability.OutcomeWatchdogHalt, terr)
				return terr
			}
			// Same symptom, different cause, and the distinction is the
			// whole of #449: this one latched nothing, so the reply must
			// not send an operator to clear a guardrail that never
			// tripped. Checked after the halt because a halt is the
			// heavier fact — if a turn somehow managed both, the session
			// is the thing that needs attention.
			if rerr := refused.why(); rerr != nil {
				ts.complete(observability.OutcomeRefusalLoop, rerr)
				return rerr
			}
			d.logger.Error("runner emitted error", "turn", label, "session", sessionID, "error", err.Error(), "events_before_error", events)
			ts.complete(observability.OutcomeError, err)
			return err
		}
		events++
		logEvent(d.logger, event, sessionID)
		if onEvent != nil {
			onEvent(event)
		}
		d.obs.Observe(event, d.workloadName)
		if berr := meter.Observe(event); berr != nil {
			tokens, cost, calls := meter.Snapshot()
			// A specialist crossing its own cap is not the session's
			// problem any more (W10.3). It used to cancel here because
			// cancelling was the only way to stop the specialist making
			// another call; since W10.2 the pre-call gate stops it, and
			// stopping one specialist is all a per-specialist ceiling ever
			// meant. The call that crossed is still folded, still priced
			// and still in the ledger — Observe did that before it
			// returned — and the coordinator gets a refusal to route
			// around on its next dispatch.
			if scope, ok := budget.Scope(berr); ok {
				d.logger.Warn("BUDGET CEILING — a specialist crossed its own cap; the session continues",
					"turn", label, "session", sessionID, "specialist", scope,
					"tokens", tokens, "cost_usd", fmt.Sprintf("%.4f", cost), "model_calls", calls,
					"error", berr.Error(),
				)
				d.obs.BudgetTrip(d.workloadName)
			} else {
				d.logger.Error("BUDGET EXCEEDED — aborting session turn",
					"turn", label, "session", sessionID,
					"tokens", tokens, "cost_usd", fmt.Sprintf("%.4f", cost), "model_calls", calls,
					"error", berr.Error(),
				)
				cancel()
				d.obs.BudgetTrip(d.workloadName)
				ts.complete(observability.OutcomeBudgetExceeded, berr)
				return berr
			}
		}
		// And the pre-call half, checked in the same place and for the
		// same reason the fold above cancels rather than waiting for the
		// stream to end: a refusal costs nothing, so nothing above it ever
		// runs out of anything. A contract loop that hands the specialist
		// its refusal back and asks again — the change-set validator does
		// exactly this — spins forever on free calls, where before W10.2
		// it burned the ceiling and stopped. The event carrying the
		// refusal has already been logged and folded by the time we get
		// here, so the reason is in the transcript before the turn unwinds.
		//
		// Only the workload's own ceiling stops the turn. A refused
		// specialist leaves the coordinator holding an answer and a roster,
		// and what bounds *that* loop is the coordinator's own calls, which
		// are real and priced against the very ceiling checked here.
		if rerr := tb.sessionRefused(d, ts, label, sessionID); rerr != nil {
			cancel()
			return rerr
		}
	}
	// A cancelled stream can also just end, without surfacing an
	// error. Either way the turn stopped because the watchdog stopped
	// it, and the caller has to hear that rather than "ok".
	if terr := enf.Preflight(); terr != nil {
		ts.complete(observability.OutcomeWatchdogHalt, terr)
		return terr
	}
	// And the same backstop for the gate's own cut, for the same reason:
	// the cancellation can land between events, and a stream that ends
	// quietly is exactly what that looks like from here.
	if rerr := refused.why(); rerr != nil {
		ts.complete(observability.OutcomeRefusalLoop, rerr)
		return rerr
	}
	// The backstop for the same check. A refusal that produced no event on
	// this stream — one inside a sub-agent whose run does not surface
	// here — never reaches the in-loop branch, and the stream then ends
	// cleanly, because that is what a refusal is. The caller asked for
	// work that did not happen; reporting OK would hide a budget stop
	// behind an answer that says "I am out of budget" in prose.
	if rerr := tb.sessionRefused(d, ts, label, sessionID); rerr != nil {
		return rerr
	}
	// The turn is finishing, so anything left over was a specialist's and
	// the workload worked around it. Reported once, here, rather than per
	// event: the meter keeps the first reason, and a fan-out refused ten
	// times says the same thing ten times.
	tb.noteScopedRefusals(d, label, sessionID)
	tokens, cost, calls := meter.Snapshot()
	d.logger.Info("turn complete", "turn", label, "session", sessionID, "events", events,
		"session_tokens", tokens, "session_cost_usd", fmt.Sprintf("%.4f", cost), "session_model_calls", calls)
	ts.complete(observability.OutcomeOK, nil)
	return nil
}

// admitTurn is the chokepoint half of runTurnPre: the refusals every turn
// kind passes before it may start, in the order an operator's deliberate
// state outranks a backstop's. It runs after the turn's cancel handle is
// registered (the register-before-check handshake) and under the turn
// lock.
func admitTurn(ctx context.Context, d turnDeps, sessionID string) error {
	// Chokepoint check, after registration. A read failure skips the
	// check (fail-open): the refusals are availability guards, and an
	// unreadable ops overlay must not wedge every session — the
	// fail-closed safety guard is the effects outbox. ErrNotFound is
	// the normal fresh-session case (the runner auto-creates).
	if det, derr := d.store.Get(ctx, "", sessionID); derr == nil {
		if det.State == transcript.StateAborted {
			return fmt.Errorf("session %q is aborted (%s); session_aborted: %w", sessionID, det.AbortReason, inject.ErrConflict)
		}
		if det.GatePause.Active() {
			return fmt.Errorf("session %q is gate-paused (%s: %s); session_paused — resume with the pause token: %w",
				sessionID, det.PauseReason, det.PauseMessage, inject.ErrConflict)
		}
	}

	// Adopt a halt a previous process recorded, before the preflight
	// that has to honor it. A daemon that crashed mid-loop restarts with
	// an empty watchdogPool, and the restart is automatic: without this
	// the loop → halt → crash → restart cycle enforce mode exists to
	// break just resumes, each restart handing the loop a clean
	// backstop. One fold per session per process; fails open.
	d.wds.restore(ctx, sessionID)

	// Watchdog halt (--watchdog=enforce): refuse before any model
	// call. The refusal has to be structural — auto-resume, a
	// scheduled fire, and an attach inject all land here, and each of
	// them would otherwise re-drive the loop that tripped it. Placed
	// after the chokepoint checks so an aborted or gate-paused session
	// still reports the state an operator set deliberately.
	if err := d.wds.preflight(sessionID); err != nil {
		return fmt.Errorf("%w: %w", inject.ErrConflict, err)
	}

	// What this session already spent, before the ceiling check that has
	// to honor it (#175). Same placement and same fail-open posture as
	// the watchdog pair above: one fold per session per process, and a
	// storage fault leaves the ceiling armed against this process's own
	// spend rather than refusing the turn.
	d.meters.restore(ctx, sessionID)
	if err := d.meters.preflight(sessionID); err != nil {
		return fmt.Errorf("%w: %w", inject.ErrConflict, err)
	}
	return nil
}

// turnBudget is what a turn has to remember about its meter from the
// moment before it ran: the refusal counts and the cost, so what the turn
// itself did is the difference. The meter is session-cumulative, and every
// report below is about this turn.
type turnBudget struct {
	meter *budget.Meter

	// What the meter had already refused before this turn. A refusal is a
	// synthesized answer rather than an error, so a turn the ceiling
	// stopped would otherwise complete looking like a turn that finished;
	// the delta is what tells the caller which it was.
	//
	// Two counts, because the two have different outcomes (W10.3). Every
	// refusal is worth reporting; only the workload's own stops the turn.
	refusalsBefore        int
	sessionRefusalsBefore int

	// Export the turn's cost delta whichever way the turn ends. The
	// meter's session-cumulative cost is authoritative (pricing lives
	// in pkg/budget); the counter only ever sees per-turn deltas.
	costBefore float64
	// And the overshoot, on the same "whichever way the turn ends"
	// footing. budget.final_report buys a stopped specialist one model
	// call past its ceiling, and a cap that was exceeded on purpose has
	// to say so out loud rather than arrive as a number that does not
	// add up (pkg/budget/finalreport.go).
	grantsBefore int
}

func newTurnBudget(meter *budget.Meter) *turnBudget {
	tb := &turnBudget{meter: meter}
	tb.refusalsBefore, _ = meter.Refusals()
	tb.sessionRefusalsBefore, _ = meter.SessionRefusals()
	_, tb.costBefore, _ = meter.Snapshot()
	tb.grantsBefore = meter.FinalReportsTaken()
	return tb
}

// sessionRefused reports a ceiling that stopped the turn itself. Same
// outcome and the same counter as the post-hoc fold, because from an
// operator's side they are the same event; the error is budget.ErrRefused
// rather than ErrExceeded because nothing was spent crossing anything.
func (tb *turnBudget) sessionRefused(d turnDeps, ts *turnSpan, label, sessionID string) error {
	meter := tb.meter
	n, first := meter.SessionRefusals()
	if n <= tb.sessionRefusalsBefore {
		return nil
	}
	tokens, cost, calls := meter.Snapshot()
	d.logger.Error("BUDGET CEILING — refused a model call before it was made",
		"turn", label, "session", sessionID,
		"tokens", tokens, "cost_usd", fmt.Sprintf("%.4f", cost), "model_calls", calls,
		"refusals", n-tb.sessionRefusalsBefore, "reason", first.Error(),
	)
	d.obs.BudgetTrip(d.workloadName)
	ts.complete(observability.OutcomeBudgetExceeded, first)
	return first
}

// noteScopedRefusals reports a specialist's own ceiling, which is not a
// stop: the refusal went back to whoever dispatched it as an answer it can
// route around, and the turn carried on. It still trips the counter and
// still says so in the log, because "one of this workload's specialists
// can no longer be used" is exactly the thing an operator alerting on
// budget trips wants to hear about, and the turn finishing is what makes
// it easy to miss.
func (tb *turnBudget) noteScopedRefusals(d turnDeps, label, sessionID string) {
	meter := tb.meter
	n, first := meter.Refusals()
	sn, _ := meter.SessionRefusals()
	scopedNew := (n - tb.refusalsBefore) - (sn - tb.sessionRefusalsBefore)
	if scopedNew <= 0 {
		return
	}
	d.logger.Warn("BUDGET CEILING — a specialist was refused; the turn routed on",
		"turn", label, "session", sessionID,
		"refusals", scopedNew, "reason", first.Error(),
	)
	d.obs.BudgetTrip(d.workloadName)
}

// finish exports the turn's cost delta and any final-report overshoot,
// whichever way the turn ended. runTurnPre defers it after the span's own
// defer, so it runs first and the span is still open.
func (tb *turnBudget) finish(d turnDeps, ts *turnSpan, label, sessionID string) {
	meter := tb.meter
	_, costAfter, _ := meter.Snapshot()
	d.obs.AddCost(d.workloadName, costAfter-tb.costBefore)
	ts.cost(costAfter - tb.costBefore)
	if granted := meter.FinalReportsTaken() - tb.grantsBefore; granted > 0 {
		d.logger.Warn("BUDGET CEILING — a stopped specialist bought its final report",
			"turn", label, "session", sessionID, "grants", granted,
			"note", "budget.final_report: one model call past the ceiling, report tool only")
	}
}

// watchdogAlertHandler is what runTurnPre hands watchdog.Tap: retain the
// alert, log it, queue its model-facing half for the next turn, and under
// enforce halt the turn in flight through cancel — the same handle a
// budget trip and an operator abort use.
func watchdogAlertHandler(d turnDeps, label, sessionID string, enf *watchdog.Enforcer, fb *watchdog.Feedback, cancel context.CancelFunc) func(watchdog.Alert) {
	return func(a watchdog.Alert) {
		// Retained as well as logged: GET /guardrails answers "has this
		// session been misbehaving?", and the alert is gone from the
		// watchdog the moment Tap hands it here.
		d.wds.note(sessionID, a)
		d.logger.Warn("watchdog alert",
			"turn", label, "session", sessionID,
			"signal", a.Signal, "severity", string(a.Severity), "reason", a.Reason)
		// Queue the model-facing half for the next turn. Not this one:
		// the prompt was assembled before Run and the turn is already
		// streaming. Under enforce that next turn is the one after an
		// operator reset, which is exactly the turn that would
		// otherwise re-issue the call that halted it.
		fb.Queue([]watchdog.Alert{a})
		// Enforce mode halts the turn in flight. Tap drains as soon as
		// an observation lands, so this runs while the loop is looping
		// rather than after it finishes — cancel() is the same handle
		// a budget trip and an operator abort use, so the turn unwinds
		// the one way the daemon already knows how to unwind.
		if enf.Observe(a) {
			_, reason := enf.Tripped()
			d.logger.Error("WATCHDOG HALT — cancelling the turn",
				"turn", label, "session", sessionID,
				"signal", a.Signal, "reason", reason)
			// Persist before cancelling. The halt has to outlive this
			// process, and the crash it is most needed for is the one
			// that follows the loop it just stopped.
			d.wds.recordTrip(sessionID, a, reason)
			cancel()
		}
	}
}
