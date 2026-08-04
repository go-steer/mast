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

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// Boot-time auto-resume (docs/durable-execution-design.md, #41): on
// startup the daemon scans for sessions a prior shutdown cut short (they
// carry a durable interruption marker on the companion ops row) and
// drives a continuation turn so unattended work finishes on its own.
//
// The guarantee is the operational form of exactly-once: auto-resume
// never double-fires a mutation. It rests on the recorded-effect outbox
// (#70) — completed effects replay from the log instead of re-executing,
// and a session carrying ANY dangling mutating intent is excluded here
// and left for an operator's ambiguous-effect ack. See the eligibility
// gate in resumeOne.
//
// Slice-1 scope (documented, follow-on in the design doc): coordinator
// dispatch only; dangling read-only calls are repaired, dangling
// delegations are deferred; a per-session restart-loop breaker plus a
// per-boot cap bound the blast radius of a poison session.
const (
	// autoResumeMaxAttempts / autoResumeAttemptWindow: skip a session
	// that has already been auto-resumed this many times within the
	// window — a turn that keeps crashing the process must not wedge
	// every boot in a restart loop (M2).
	autoResumeMaxAttempts   = 3
	autoResumeAttemptWindow = 10 * time.Minute
	// autoResumeBootCap bounds the total number of continuation turns a
	// single boot will drive, so a store full of poison sessions cannot
	// monopolize a restart.
	autoResumeBootCap = 50
)

// errAutoResumeSuperseded is returned by the preTurn supersession hook
// when a concurrent inject/resume advanced the session (or moved it out
// of StateInterrupted) between the boot scan and the turn — the M1 TOCTOU
// guard. It is distinguished from a real turn failure so the outcome is
// reported as skipped, not error.
var errAutoResumeSuperseded = errors.New("auto-resume: session superseded since scan")

// autoResumer holds the daemon plumbing the boot pass drives turns
// through. It reuses the exact chokepoint (runTurnPre) every other turn
// kind uses, so aborted/paused refusal, the per-session turn lock, the
// budget meter, and the effects outbox backstop all apply unchanged.
type autoResumer struct {
	runner       *runner.Runner
	logger       *slog.Logger
	store        *transcript.Store
	meters       *meterPool
	wds          *watchdogPool
	obs          *observability.Registry
	tracker      *turnTracker
	turnLocks    *sessionTurnLocks
	workloadName string
	bundle       *workload.Bundle
	dispatchMode string
	// pred / subAgents are the same effects classification the outbox
	// plugin was built with — shared so the eligibility gate can never
	// drift from what the outbox considers dangling.
	pred      effects.Predicate
	subAgents map[string]bool
	window    time.Duration
}

// run is the boot pass: scan interrupted sessions and drive a
// continuation for each eligible one, sequentially, on ctx (turnCtx —
// so a shutdown drain cancels the pass mid-flight). Sequential is
// deliberate: the per-session turn lock would serialize same-session
// work anyway, and a bounded, ordered pass is easier to reason about
// than a fan-out under a restart.
func (a *autoResumer) run(ctx context.Context) {
	candidates, err := a.store.ScanInterrupted(ctx)
	if err != nil {
		a.logger.Error("auto-resume boot scan failed; interrupted sessions will not resume until next restart",
			"error", err.Error())
		return
	}
	if len(candidates) == 0 {
		a.logger.Debug("auto-resume boot scan: no interrupted sessions")
		return
	}
	a.logger.Info("auto-resume boot scan", "candidates", len(candidates), "window", a.window.String())
	turnsThisBoot := 0
	for _, c := range candidates {
		// Stop launching new turns the moment a drain begins — the same
		// contract every other turn-launcher honors (inject, resume,
		// attach, the timed-pause scheduler). turnCtx is only cancelled at
		// drain EXPIRY, so ctx.Err() alone would keep starting turns
		// through the whole drain window; isDraining() is the early gate,
		// and the drain path awaits this goroutine's completion.
		if ctx.Err() != nil || a.tracker.isDraining() {
			a.logger.Info("auto-resume pass cut short by shutdown")
			return
		}
		outcome := a.resumeOne(ctx, c, &turnsThisBoot)
		a.obs.AutoResume(a.workloadName, outcome)
		a.logger.Info("auto-resume decision", "session", c.SessionID, "outcome", outcome)
	}
}

// resumeOne applies the #41 decision tree to one interrupted candidate
// and returns the observability outcome. It never panics on a per-session
// error: a failure leaves the marker in place for the next boot or an
// operator, so the pass keeps moving.
func (a *autoResumer) resumeOne(ctx context.Context, c transcript.InterruptedCandidate, turnsThisBoot *int) string {
	log := a.logger.With("session", c.SessionID, "reason", c.InterruptReason)

	// runTurnPre drives under the daemon's fixed user ID; a session owned
	// by a different user id was not created by this daemon's turn path
	// and is not ours to resume.
	if c.UserID != defaultUserID {
		log.Warn("auto-resume skipped: session owned by a different user id", "user", c.UserID)
		return observability.AutoResumeSkippedUnsupported
	}

	// 1. Freshness — use the ORIGINAL interruption time so a long outage
	//    correctly skips work that was already stale when the crash hit.
	//    A zero/unparseable InterruptedAt reads as infinitely old, the
	//    safe direction (don't resume what we cannot date).
	if a.window > 0 && time.Since(c.InterruptedAt) > a.window {
		log.Info("auto-resume skipped: interruption older than freshness window",
			"interrupted_at", c.InterruptedAt.Format(time.RFC3339), "window", a.window.String())
		return observability.AutoResumeSkippedStale
	}

	// 2. Dispatch scope — the graph/workflowagent turn-driving path is
	//    unverified for interrupted (non-paused) sessions; slice-1 is
	//    coordinator-only.
	if a.dispatchMode != "coordinator" {
		log.Info("auto-resume skipped: dispatch mode not supported in slice-1", "dispatch", a.dispatchMode)
		return observability.AutoResumeSkippedUnsupported
	}

	// 3. Eligibility gate (H1) — the once-and-only-once guarantee. A
	//    dangling MUTATING intent means an effect may or may not have
	//    committed: excluded regardless of any ack watermark (an ack
	//    suppresses the outbox refusal but does not pair the call, so the
	//    unpaired tool_use would still hit the provider, and synthesizing
	//    a response would falsely claim the effect did not happen).
	//    Operator territory. A dangling DELEGATION/control call is
	//    engine-reconstruct territory the slice does not drive.
	scan := effects.ScanDangling(c.Events, a.pred, a.subAgents)
	if len(scan.Mutating) > 0 {
		log.Warn("auto-resume skipped: dangling mutating intent (ambiguous effect); operator ack required",
			"dangling_mutating", len(scan.Mutating))
		return observability.AutoResumeSkippedAmbiguous
	}
	if len(scan.Deferred) > 0 {
		log.Info("auto-resume skipped: dangling delegation/control call not supported in slice-1",
			"deferred", len(scan.Deferred))
		return observability.AutoResumeSkippedUnsupported
	}

	// 4. Classify the trailing event (H2) to choose how to drive.
	//    A trailing event authored by a sub-agent means the turn was cut
	//    short mid-delegation. At the coordinator level ADK filters and
	//    converts foreign-branch events, so neither a synthetic repair
	//    (its call event may not be visible in the coordinator's rebuilt
	//    history — the turn would error) nor a raw-role classification is
	//    reliable there; slice-1 drives coordinator-authored turns only.
	trailing := trailingEvent(c.Events)
	if trailing != nil && a.subAgents[trailing.Author] {
		log.Info("auto-resume skipped: trailing event is sub-agent-authored (mid-delegation); not supported in slice-1",
			"author", trailing.Author)
		return observability.AutoResumeSkippedUnsupported
	}

	var msg *genai.Content // nil = re-invoke the model over history (Case B)
	if len(scan.Repairable) > 0 {
		// Case A: a read-only tool was cut off mid-flight. Answer it with
		// a synthetic error response so the history is provider-valid,
		// then re-run. The repair must cover exactly the single last
		// function-call event (H3): a repairable call in an earlier event
		// would make the message span multiple function-call events,
		// which ADK's latest-function-response rearrangement rejects.
		for _, d := range scan.Repairable {
			if d.EventIndex != scan.LastCallEventIndex {
				log.Info("auto-resume skipped: repairable calls span multiple events",
					"last_call_event", scan.LastCallEventIndex)
				return observability.AutoResumeSkippedUnsupported
			}
		}
		msg = repairContent(scan.Repairable)
	} else if trailing != nil && trailing.Content != nil && trailing.Content.Role == genai.RoleModel {
		// No dangling calls and the transcript already ends on a model
		// turn: the interrupted turn actually finished (a stale marker /
		// clear race). Re-invoking with a nil msg would inject ADK's
		// synthetic "Continue processing…" user turn and fabricate fresh,
		// possibly-mutating work — so just clear the marker, no turn.
		if err := a.store.ClearInterrupted(ctx, c.UserID, c.SessionID); err != nil {
			log.Error("auto-resume: clearing stale interruption marker failed", "error", err.Error())
			return observability.AutoResumeError
		}
		log.Info("auto-resume cleared stale interruption marker (turn had already completed)")
		return observability.AutoResumeCleared
	}
	// else Case B: trailing user turn or paired tool result — nil msg
	// re-invokes the model over history without a synthetic "Continue".

	// 5. Restart-loop breaker (M2) — per session, then per boot.
	attempts, last := a.store.AutoResumeAttempts(ctx, c.UserID, c.SessionID)
	if attempts >= autoResumeMaxAttempts && time.Since(last) < autoResumeAttemptWindow {
		log.Warn("auto-resume skipped: restart-loop breaker tripped",
			"attempts", attempts, "window", autoResumeAttemptWindow.String())
		return observability.AutoResumeSkippedLoopbreak
	}
	if *turnsThisBoot >= autoResumeBootCap {
		log.Warn("auto-resume skipped: per-boot turn cap reached", "cap", autoResumeBootCap)
		return observability.AutoResumeSkippedLoopbreak
	}

	// 6. Durably count the attempt BEFORE running (M2): a turn that
	//    panics or SIGSEGVs the whole process — the exact restart-loop
	//    threat — must still have been counted. A refusal that does not
	//    run a turn (supersession, chokepoint conflict) over-counts. When
	//    a concurrent inject advances the session but leaves the marker,
	//    the session still projects interrupted and the over-count IS
	//    re-seen next boot — but only in the safe direction: it can at
	//    worst trip the per-session breaker sooner (a skip, never a
	//    resume), and it self-heals once the attempt window rolls over.
	if _, err := a.store.RecordAutoResumeAttempt(ctx, c.UserID, c.SessionID); err != nil {
		log.Error("auto-resume: recording the attempt failed; not running (the loop breaker would be blind)",
			"error", err.Error())
		return observability.AutoResumeError
	}
	*turnsThisBoot++

	// 7. Supersession recheck (M1) — runs under the turn lock, before the
	//    turn. The chokepoint refuses aborted/gate-paused sessions but NOT
	//    StateInterrupted, so this closes the TOCTOU where a concurrent
	//    inject/resume advanced the session between scan and drive.
	preTurn := func(ctx context.Context) error {
		d, err := a.store.Get(ctx, c.UserID, c.SessionID)
		if err != nil {
			return fmt.Errorf("%w: recheck get failed: %v", errAutoResumeSuperseded, err)
		}
		if d.State != transcript.StateInterrupted {
			return fmt.Errorf("%w: state is now %q", errAutoResumeSuperseded, d.State)
		}
		if d.EventCount != c.EventCount || !d.LastEventTime.Equal(c.LastEventTime) {
			return fmt.Errorf("%w: session advanced since scan (events %d→%d)",
				errAutoResumeSuperseded, c.EventCount, d.EventCount)
		}
		return nil
	}

	// 8. Drive through the shared chokepoint.
	label := "autoresume:" + c.SessionID
	err := runTurnPre(ctx, a.runner, a.logger, a.store, a.meters, a.wds, a.obs,
		a.tracker, a.turnLocks, a.workloadName, c.SessionID, msg, label, preTurn)
	switch {
	case err == nil:
		// The turn finished within (a fresh) boot: clear both markers so a
		// later interruption starts clean. A clear failure here is benign —
		// next boot re-derives a clean model turn and clears it then.
		if cerr := a.store.ClearInterrupted(ctx, c.UserID, c.SessionID); cerr != nil {
			log.Error("auto-resume ran but clearing the interruption marker failed", "error", cerr.Error())
		}
		if cerr := a.store.ClearAutoResumeAttempts(ctx, c.UserID, c.SessionID); cerr != nil {
			log.Warn("auto-resume: clearing the attempt counter failed", "error", cerr.Error())
		}
		log.Info("auto-resume completed a continuation turn")
		return observability.AutoResumeResumed
	case errors.Is(err, errAutoResumeSuperseded):
		log.Info("auto-resume skipped: session superseded before the turn", "detail", err.Error())
		return observability.AutoResumeSkippedSuperseded
	case errors.Is(err, inject.ErrConflict):
		// The chokepoint refused: the session went aborted/gate-paused
		// between scan and drive. Not a turn failure — the session left
		// the eligible set.
		log.Info("auto-resume skipped: session became terminal or paused before the turn", "detail", err.Error())
		return observability.AutoResumeSkippedSuperseded
	default:
		log.Error("auto-resume turn failed; leaving the interruption marker for retry or operator",
			"error", err.Error())
		return observability.AutoResumeError
	}
}

// repairContent builds the H3 repair message: one synthetic error
// FunctionResponse per dangling read-only call in the last function-call
// event. Each carries the original call ID (pairing is by ID) and the
// tool name (Gemini needs the name to match).
func repairContent(repairable []effects.DanglingIntent) *genai.Content {
	parts := make([]*genai.Part, 0, len(repairable))
	for _, d := range repairable {
		p := genai.NewPartFromFunctionResponse(d.ToolName, map[string]any{
			"error": "interrupted before completion",
		})
		p.FunctionResponse.ID = d.CallID
		parts = append(parts, p)
	}
	return &genai.Content{Role: genai.RoleUser, Parts: parts}
}

// trailingEvent returns the last event that carries content ADK would
// keep when it rebuilds prompt history, or nil if the session has none.
// It skips nil-content and empty-role events — the cheap, always-correct
// subset of ADK's buildContentsDefault filtering — so the caller reads
// the true last visible role (model turn = the interrupted turn actually
// finished; user turn / tool result = a genuine mid-turn interruption)
// and the true last visible author (a sub-agent author = mid-delegation).
// The richer exclusions (branch/isolation-scope/EUC) only bite in shapes
// slice-1 does not drive and are a documented follow-on.
func trailingEvent(events session.Events) *session.Event {
	var last *session.Event
	for ev := range events.All() {
		if ev == nil || ev.Content == nil || ev.Content.Role == "" {
			continue
		}
		last = ev
	}
	return last
}
