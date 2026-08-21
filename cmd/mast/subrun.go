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
	"log/slog"
	"sync"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/watchdog"
)

// daemonSubRunObserver folds the events of a planner dispatch's private
// sub-runner into the three consumers the daemon's own turn loop feeds
// (#226): the session's budget meter, the metric registry, and the
// watchdog.
//
// A planner runs each invoke_specialist dispatch on a runner of its
// own, and a private runner is a private event stream — through v0.4 a
// dispatched specialist's model calls reached none of the three, which
// meant a workload with a $5 ceiling could spend past it as long as it
// spent through the planner's door, and a specialist looping inside a
// dispatch could not be halted at all. Coordinator, graph and fan-out
// dispatch never had the gap: those funnel sub-agent events up the root
// stream, where the turn loop already observes them.
//
// # A sink per dispatch
//
// The meter and the registry are cumulative and would be happy with a
// flat callback. The watchdog is not: it dedups an aggregator's
// re-emissions within a run and counts repetition across runs, so it
// needs to know where one dispatch ends and the next begins — which is
// why planner hands out a sink per dispatch rather than calling one
// method per event.
//
// # Late binding
//
// buildRoot runs before the meter pool, the registry, the watchdog pool
// and the turn tracker exist (each needs something the root is built
// from), so the observer is constructed empty, handed to compose, and
// given its sinks by attach once they exist. Same shape as
// daemonPauseRecorder, and for the same reason. Until attach runs there
// is nothing to observe: no turn has started, so no dispatch can be in
// flight.
type daemonSubRunObserver struct {
	mu       sync.RWMutex
	meters   *meterPool
	obs      *observability.Registry
	wds      *watchdogPool
	tracker  *turnTracker
	workload string
	logger   *slog.Logger

	// unattributed fires once, for the sub-run event that arrives with
	// no outer session to bill. It should be unreachable — every
	// dispatch runs inside a turn, and a turn has a session — but the
	// alternative to noticing is metering a session named "", which
	// would look exactly like a workload that never spends.
	unattributed sync.Once
}

// attach gives the observer its sinks. Called once, after the meter
// pool, the registry, the watchdog pool and the turn tracker are built.
func (o *daemonSubRunObserver) attach(meters *meterPool, obs *observability.Registry, wds *watchdogPool, tracker *turnTracker, workload string, logger *slog.Logger) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.meters, o.obs, o.wds, o.tracker = meters, obs, wds, tracker
	o.workload, o.logger = workload, logger
}

// SubRun implements planner.SubRunObserver: one sink per dispatch,
// holding the sinks as they stood when the dispatch opened.
//
// A dispatch that opens before attach gets an inert sink rather than
// nil, because the two are not the same statement — nil says "this host
// does not observe dispatches", which is a claim about the host, and
// this one does.
func (o *daemonSubRunObserver) SubRun(sessionID, specialist string) planner.SubRunSink {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return &daemonSubRun{
		owner:      o,
		sessionID:  sessionID,
		specialist: specialist,
		meters:     o.meters,
		obs:        o.obs,
		wds:        o.wds,
		tracker:    o.tracker,
		workload:   o.workload,
		logger:     o.logger,
		// Dedup scoped to this dispatch, exactly as watchdog.Tap scopes
		// one to a turn: an aggregator re-emitting a FunctionCall part
		// must not count twice, and the same call in the NEXT dispatch
		// must count again — cross-run repetition is the signal.
		seen: map[string]struct{}{},
	}
}

// daemonSubRun is one dispatch's sink.
//
// Not safe for concurrent use, and it does not need to be: a sink
// belongs to one dispatch, and one dispatch is one sub-runner's stream.
// Two dispatches running in parallel hold two sinks. What they share —
// the meter, the registry, the session's watchdog and enforcer — is
// each independently safe for concurrent use.
type daemonSubRun struct {
	owner      *daemonSubRunObserver
	sessionID  string
	specialist string

	meters   *meterPool
	obs      *observability.Registry
	wds      *watchdogPool
	tracker  *turnTracker
	workload string
	logger   *slog.Logger

	seen   map[string]struct{}
	halted error
}

// Observe implements planner.SubRunSink.
//
// The returned error stops the sub-run and hands the planner a labelled
// partial, so a specialist that crosses a ceiling costs the dispatch
// rather than the session. That is the finer-grained stop pkg/budget's
// package doc says the event-stream seam cannot give: here the run
// being stopped IS the specialist's.
//
// The three consumers run in a deliberate order. Metrics first, because
// the call that crosses a ceiling still happened and still cost money —
// a token count that omits the last call is the one an operator
// reconciling a provider bill would trip over. Then the watchdog, whose
// verdict is about behaviour and outranks the budget's about spend: a
// session the watchdog halts is refused until an operator resets it,
// where a budget stop is cleared by raising the ceiling. Then the
// meter.
func (r *daemonSubRun) Observe(ev *session.Event) error {
	if ev == nil {
		return nil
	}
	r.obs.Observe(ev, r.workload)
	if err := r.watch(ev); err != nil {
		return err
	}
	if r.meters == nil {
		return nil
	}
	if r.sessionID == "" {
		r.owner.unattributed.Do(func() {
			if r.logger != nil {
				r.logger.Error("planner sub-run event arrived with no session id; its spend is not metered",
					"workload", r.workload, "author", ev.Author)
			}
		})
		return nil
	}
	return r.meters.meter(r.sessionID).Observe(ev)
}

// Close implements planner.SubRunSink: the end-of-run drain, the same
// one watchdog.Tap defers at the end of a turn. A signal that tripped
// on the dispatch's last event would otherwise sit in the watchdog
// until the next observation, which on a session whose last act was
// that dispatch is never.
//
// It cannot stop the dispatch — that is over — but it can still trip
// the session, which is the halt that matters: the trip is a latch, and
// the turn that reads it is the next one.
func (r *daemonSubRun) Close() {
	if r.wds == nil || r.sessionID == "" {
		return
	}
	watchdog.Drain(r.wds.watchdog(r.sessionID), r.onAlert)
}

// watch feeds one sub-run event to the session's watchdog and returns
// the halt, if this event produced one.
//
// # Why the session's watchdog and not the dispatch's own
//
// The signals count repetition, and a watchdog minted per dispatch
// resets that count every time a dispatch ends: a specialist making
// three identical calls in each of ten dispatches would never reach a
// threshold of five. The session is the scope the watchdog has always
// had — its state already survives turn boundaries for the same reason
// — and a dispatch is smaller than a turn.
//
// The cost of that choice is honest to state: the planner's own tool
// calls and a specialist's now land in one signal set, so an
// invoke_specialist call between two dispatches breaks a consecutive
// run the repeat detector was building. That is the interleave shape
// DominantToolCallSignal (#227) exists for, and it is the same
// trade-off the outer stream already makes between a coordinator's
// calls and its sub-agents'. The loop this exists to catch — a
// specialist spinning inside ONE dispatch — has no interleaver at all.
//
// # Why a trip stops the session and not just the dispatch
//
// The budget half stops only the sub-run, and is right to: a ceiling is
// cumulative, the outer meter is over the line by the time the sub-run
// is, and the planner's next round trips the session through the normal
// path. A watchdog trip is not cumulative. It is a latch that means
// "this session is behaving pathologically and an operator must reset
// it", and every other door to it — the outer stream's alert path,
// Preflight on the next turn, a restored trip after a restart — refuses
// the whole session. A halt that only stopped the dispatch would let
// the planner re-dispatch the same specialist immediately, which is the
// treadmill enforce mode exists to break.
//
// So both levers fire: the sub-run is stopped with a labelled partial
// (the planner is told), and the turn is cancelled through the same
// handle an operator abort and a budget trip use (the session is
// halted). Under warn and feedback nothing is cancelled, because
// Enforcer.Observe reports a halt only under enforce.
func (r *daemonSubRun) watch(ev *session.Event) error {
	if r.wds == nil || r.sessionID == "" {
		return nil
	}
	watchdog.ObserveInto(r.wds.watchdog(r.sessionID), ev, r.seen, r.onAlert)
	return r.halted
}

// onAlert is the dispatch's copy of the turn loop's alert handler. It
// does the same four things — retain, log, queue the model-facing half
// for the next turn, and halt under enforce — differing only in what it
// has to cancel, and in saying which specialist was running.
func (r *daemonSubRun) onAlert(a watchdog.Alert) {
	// Retained as well as logged: GET /guardrails answers "has this
	// session been misbehaving?", and a dispatch's alerts are as much
	// an answer to that as a planner's.
	r.wds.note(r.sessionID, a)
	if r.logger != nil {
		r.logger.Warn("watchdog alert inside a planner dispatch",
			"session", r.sessionID, "specialist", r.specialist,
			"signal", a.Signal, "severity", string(a.Severity), "reason", a.Reason)
	}
	// The party that can stop making the looping call is the model
	// making it — but the model that gets this back is the PLANNER, not
	// the specialist. The specialist's sub-session is in-memory and
	// gone; the planner is the one still running, the one that chose to
	// dispatch, and the one that would otherwise dispatch again.
	r.wds.feedback(r.sessionID).Queue([]watchdog.Alert{a})
	enf := r.wds.enforcer(r.sessionID)
	if !enf.Observe(a) {
		return
	}
	_, reason := enf.Tripped()
	if r.logger != nil {
		r.logger.Error("WATCHDOG HALT — stopping the dispatch and cancelling the turn",
			"session", r.sessionID, "specialist", r.specialist,
			"signal", a.Signal, "reason", reason)
	}
	// Persist before cancelling, as the turn loop does: the halt has to
	// outlive this process, and the crash it is most needed for is the
	// one that follows the loop it just stopped.
	r.wds.recordTrip(r.sessionID, a, reason)
	r.halted = enf.Preflight()
	if r.tracker != nil {
		r.tracker.cancelSession(r.sessionID)
	}
}
