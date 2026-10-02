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
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-steer/mast/internal/attach"
	"github.com/go-steer/mast/internal/eventlog"
	"github.com/go-steer/mast/pkg/watchdog"
)

// watchdogPool hands out one watchdog per session, mirroring
// meterPool: the repeated-tool-call signal counts consecutive
// identical calls across turns, so its state must live with the
// session, not the turn — and a daemon-global watchdog would blend
// unrelated sessions' tool streams into false positives.
type watchdogPool struct {
	mu   sync.Mutex
	mode watchdog.Mode
	byID map[string]*watchdog.DefaultWatchdog
	// fired retains what Check() drains. watchdog.Tap collects alerts
	// as they trigger and hands them straight to the log, so by the
	// time an operator asks the watchdog itself remembers nothing —
	// and "armed, 0 alerts" is the same answer for a healthy session
	// and one that looped six times an hour ago.
	fired map[string]watchdogAlerts
	// enf holds the per-session halt state under --watchdog=enforce.
	// Separate from byID because a trip outlives the signal run that
	// caused it: the signal is reset the moment the operator clears
	// the guardrail, and the refusal has to survive until then.
	enf map[string]*watchdog.Enforcer
	// fb holds the per-session queue of observations awaiting delivery
	// to the model, from --watchdog=feedback up.
	fb map[string]*watchdog.Feedback

	// store persists trips and resets so a halt survives the process
	// that observed it. Nil when the daemon has no durable session
	// store, in which case every method below degrades to the
	// in-memory behavior — a *eventlog.GuardrailStore is nil-safe.
	store *eventlog.GuardrailStore
	app   string
	user  string
	// restored latches per session once the durable state has been
	// folded in, so the fold is one read per session per process rather
	// than one per turn. Set on success only: a read that failed must be
	// retried on the next turn, not remembered as "restored to nothing".
	restored map[string]bool
	logger   *slog.Logger
}

// watchdogAlerts is the operator-visible residue of a session's
// alerts: how many have fired since the last reset, and the latest
// one's text.
type watchdogAlerts struct {
	count int
	last  string
}

func newWatchdogPool(mode watchdog.Mode) *watchdogPool {
	if mode == "" {
		mode = watchdog.ModeWarn
	}
	return &watchdogPool{
		mode:     mode,
		byID:     map[string]*watchdog.DefaultWatchdog{},
		fired:    map[string]watchdogAlerts{},
		enf:      map[string]*watchdog.Enforcer{},
		fb:       map[string]*watchdog.Feedback{},
		restored: map[string]bool{},
	}
}

// durable gives the pool somewhere to write its trips, so a halt
// outlives the process that observed it.
//
// Wired only when the daemon has an attach listener, which is the only
// surface with a reset endpoint. A persisted halt with no way to clear
// it is not a backstop, it is a brick: the operator's recourse would be
// deleting a database row. --attach-listen implies --session-db
// (#329), so the store exists exactly when the reset does.
// The (app, user) the rows are keyed on are the daemon's own constants,
// the same pair the attach wiring and the transcript store use: a
// single-workload process has exactly one of each, and threading them
// through as parameters would suggest a choice that does not exist.
func (wp *watchdogPool) durable(store *eventlog.GuardrailStore, logger *slog.Logger) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	wp.store, wp.app, wp.user, wp.logger = store, appName, defaultUserID, logger
}

// restore folds the session's persisted guardrail state into this
// process's enforcer, once per session.
//
// Called at the top of every turn rather than when the session is
// minted, because a session's first appearance in a fresh daemon is a
// turn: auto-resume, a scheduled fire, and an attach inject all reach
// runTurnPre, and each of them is a way for a halted session to start
// running again after a restart.
//
// Fails open, loudly. A guardrail that cannot be read is a reason to
// log and continue, not a reason to refuse work: a corrupt or
// locked-out row would otherwise halt every session in the deployment
// with no trip behind it, converting a storage fault into an outage.
// The turn-level backstops — the budget ceiling, the tool-failure
// streak — are all still armed, and the halt re-trips the moment the
// loop that caused it recurs.
func (wp *watchdogPool) restore(ctx context.Context, sessionID string) {
	wp.mu.Lock()
	store, app, user, done, logger := wp.store, wp.app, wp.user, wp.restored[sessionID], wp.logger
	wp.mu.Unlock()
	if store == nil || done {
		return
	}
	st, err := store.Fold(ctx, app, user, sessionID)
	if err != nil {
		if logger != nil {
			logger.Warn("could not read persisted guardrail state; continuing without it",
				"session", sessionID, "error", err.Error())
		}
		return
	}
	wp.mu.Lock()
	wp.restored[sessionID] = true
	wp.mu.Unlock()
	if !st.WatchdogTripped {
		return
	}
	// Adopt is the one that decides: it refuses unless the current mode
	// enforces, so a deployment dialed back to feedback does not inherit
	// a halt it would no longer produce.
	if !wp.enforcer(sessionID).Adopt(st.WatchdogSignal, st.WatchdogReason) {
		return
	}
	if logger != nil {
		logger.Warn("restored a watchdog halt recorded before this process started",
			"session", sessionID, "signal", st.WatchdogSignal, "tripped_at", st.TrippedAt.Format(time.RFC3339))
	}
}

// recordTrip persists a halt. Best-effort and background-context: the
// turn's own context is being cancelled by this very halt, and a row
// written because a backstop fired must not be lost to the cancellation
// the backstop caused.
func (wp *watchdogPool) recordTrip(sessionID string, a watchdog.Alert, reason string) {
	wp.mu.Lock()
	store, app, user, logger := wp.store, wp.app, wp.user, wp.logger
	wp.mu.Unlock()
	if store == nil {
		return
	}
	err := store.Append(context.Background(), app, user, sessionID, eventlog.GuardrailRecord{
		Kind:      eventlog.GuardrailKindTrip,
		Guardrail: eventlog.GuardrailWatchdog,
		Signal:    a.Signal,
		Reason:    reason,
	})
	if err != nil && logger != nil {
		// The session is halted either way — the in-memory enforcer
		// already latched. What is lost is only the halt's survival of a
		// restart, which is worth saying out loud.
		logger.Error("watchdog halt is not durable: could not persist the trip",
			"session", sessionID, "signal", a.Signal, "error", err.Error())
	}
}

func (wp *watchdogPool) watchdog(sessionID string) *watchdog.DefaultWatchdog {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	w, ok := wp.byID[sessionID]
	if !ok {
		w = watchdog.NewDefaultWatchdog()
		wp.byID[sessionID] = w
	}
	return w
}

// enforcer returns the session's halt state, minting one in the pool's
// configured posture.
func (wp *watchdogPool) enforcer(sessionID string) *watchdog.Enforcer {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	e, ok := wp.enf[sessionID]
	if !ok {
		// The remedy names the session's own reset path, not a
		// placeholder: an operator reading a halt reason out of a log
		// line has the curl they need without looking anything up.
		e = watchdog.NewEnforcer(wp.mode, fmt.Sprintf(
			"The session refuses new turns until an operator resets it (POST /sessions/%s/guardrails/reset).",
			sessionID))
		wp.enf[sessionID] = e
	}
	return e
}

// feedback returns the session's pending-observation queue, minting one
// in the pool's configured posture. Below ModeFeedback the queue exists
// but never accepts anything, so callers need no mode check of their
// own.
func (wp *watchdogPool) feedback(sessionID string) *watchdog.Feedback {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	f, ok := wp.fb[sessionID]
	if !ok {
		f = watchdog.NewFeedback(wp.mode)
		wp.fb[sessionID] = f
	}
	return f
}

// preflight refuses a turn on a session the watchdog halted. Called at
// the top of every turn, before any model call — auto-resume, the
// scheduler, and an attach inject all reach runTurnPre, and each of
// them would otherwise re-drive the loop that tripped it.
func (wp *watchdogPool) preflight(sessionID string) error {
	return wp.enforcer(sessionID).Preflight()
}

// halted reports whether the session is refusing turns, and why.
func (wp *watchdogPool) halted(sessionID string) (bool, string) {
	return wp.enforcer(sessionID).Tripped()
}

// note records one fired alert for the guardrail projection.
func (wp *watchdogPool) note(sessionID string, a watchdog.Alert) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	f := wp.fired[sessionID]
	f.count++
	f.last = a.Reason
	wp.fired[sessionID] = f
}

// alerts reports the session's accumulated alerts.
func (wp *watchdogPool) alerts(sessionID string) watchdogAlerts {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return wp.fired[sessionID]
}

// reset clears the session's signal state, its halt, and its alert
// residue — what POST /guardrails/reset does to the watchdog half.
//
// All three together, deliberately. Clearing the trip while the signal
// still holds the completed run would re-halt on the session's next
// tool call, and the operator would read that as the reset not
// working.
//
// The pending feedback queue is just as deliberately left alone. A
// reset resumes a model whose context still ends in the loop it was
// halted for; the queued observation is the only thing standing between
// that and the first post-reset turn re-issuing the same call. Clearing
// it here would make the reset undo the correction along with the halt,
// and turn an operator round-trip into a treadmill.
func (wp *watchdogPool) reset(sessionID string) {
	wp.watchdog(sessionID).Reset()
	wp.enforcer(sessionID).Reset()
	wp.mu.Lock()
	defer wp.mu.Unlock()
	delete(wp.fired, sessionID)
}

// forgetToolRun clears the session's signal state and nothing else,
// for the write gate to call when it turns away a re-proposal of a
// call an operator already refused (#449).
//
// The gate and the watchdog watch the same behaviour from two sides,
// and the calls the gate suppresses never happen — nothing was
// executed and nobody was asked twice. Leaving them on the watchdog's
// books means the repeated-call signal keeps counting toward a session
// halt that the gate has already made unnecessary, and the operator
// pays for the model's loop with a guardrail reset.
//
// Signals only, and the halt deliberately untouched. An arm that could
// un-halt would let a looping agent overrule the operator by looping
// harder: propose, get suppressed, clear the trip, repeat. Clearing the
// evidence is safe because the evidence is about calls that did not
// happen; clearing the verdict is not. Contrast reset above, which is
// an operator's own instruction and clears all three.
func (wp *watchdogPool) forgetToolRun(sessionID string) {
	wp.watchdog(sessionID).Reset()
}

// recordReset persists an operator's reset, which both clears the
// durable halt and serves as the audit record for the intervention.
//
// One row, not two. A separate "the operator did this" row alongside a
// "the halt is cleared" row would be two things to keep in agreement,
// and the interesting audit question — who cleared it, when, and what
// runway did they hand over — is answered by the row that does the
// clearing.
//
// The scope a grant was aimed at is recorded alongside it, and this is
// the seam that made grant replay possible (#175). #166 recorded the
// grants for the audit trail and did not replay them, because raising a
// ceiling over an accumulator that had forgotten what it spent is
// arithmetic on a number that no longer means anything. Durable spend
// inverted that, and replaying a specialist's grant onto the session
// would raise a cap by an amount nobody granted it — so the scope has to
// be a fact in the row rather than an assumption in the reader. Rows
// written before this column carry NULL and are not replayed at all; see
// eventlog.GuardrailRecord.GrantScope.
func (wp *watchdogPool) recordReset(sessionID, guardrail, caller, scope string, resp attach.GuardrailResetResponse) {
	wp.mu.Lock()
	store, app, user, logger := wp.store, wp.app, wp.user, wp.logger
	wp.mu.Unlock()
	if store == nil {
		return
	}
	err := store.Append(context.Background(), app, user, sessionID, eventlog.GuardrailRecord{
		Kind:           eventlog.GuardrailKindReset,
		Guardrail:      guardrail,
		Caller:         caller,
		Reason:         resp.Message,
		BudgetAddedUSD: resp.BudgetAddedUSD,
		TokensAdded:    resp.TokensAdded,
		TurnsAdded:     resp.TurnsAdded,
		GrantScope:     &scope,
	})
	if err != nil && logger != nil {
		// Worth an error, not a failed request: the operator's reset
		// took effect in this process. What did not happen is the
		// durable clear, so a restart would restore a halt they already
		// cleared — and they need to know that before the restart, not
		// after it.
		logger.Error("guardrail reset is not durable: could not persist it",
			"session", sessionID, "guardrail", guardrail, "error", err.Error())
	}
}
