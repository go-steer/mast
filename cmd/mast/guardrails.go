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
	"fmt"
	"log/slog"
	"strings"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/budget"
)

// guardrailView projects the daemon's two runtime backstops onto the
// attach guardrail surface, and services the reset (#135).
//
// The projection is derived, never cached: pkg/budget's meter decides
// enforcement by comparing accumulator against ceiling on every event,
// so any state this file kept alongside it could disagree with the
// thing that actually stops the run. Both halves — read and reset —
// go through the same budget.Trip reporting the meter uses.
type guardrailView struct {
	meters *meterPool
	wds    *watchdogPool
	logger *slog.Logger
}

// mast ships two watchdog postures, selected by --watchdog: warn logs
// and lets the turn run, enforce cancels the turn in flight on a
// Critical alert and refuses the session's next one. The prompt and
// feedback modes in the design doc are still deferred.

// info renders GET /sessions/{sid}/guardrails.
//
// It resolves the session's meter through the pool, which mints one if
// the session hasn't run a turn yet. That is deliberate: the meter is
// where a pre-emptive grant has to land, so the read and the reset
// must be looking at the same object the next turn will meter against.
func (g *guardrailView) info(sid string) attach.GuardrailInfo {
	// A halt recorded before this process started is still a halt, and
	// an operator polling a freshly restarted daemon must not be told
	// the session is healthy right up until the next turn refuses. The
	// fold latches per session, so this is one read, not one per poll.
	//
	// Background context because attach.GuardrailProvider takes none —
	// changing that public signature to thread one through is a bigger
	// change than this read is worth.
	g.wds.restore(context.Background(), sid)
	// And the spend half (#175): an operator polling a restarted daemon
	// must not be shown $0.00 against a $5.00 cap on a session that has
	// already spent $5.02. The projection below reads the meter, so the
	// meter has to have caught up first.
	g.meters.restore(context.Background(), sid)

	m := g.meters.meter(sid)
	tokens, cost, calls := m.Snapshot()
	lim := m.SessionLimits()
	trips := m.Trips()

	cc := attach.CostCeilingInfo{
		// MaxTurnUSD stays 0 — mast has no per-turn dollar bound.
		MaxSessionUSD:  lim.MaxCostUSD,
		SessionCostUSD: cost,
		MaxTokens:      lim.MaxTokens,
		Tokens:         tokens,
		MaxTurns:       lim.MaxTurns,
		Turns:          calls,
		Scopes:         g.scopes(m, trips),
	}
	// Tripped is the session's own ceilings and not the roster's (W10.3).
	// It used to cover the scopes because a specialist's crossed ceiling
	// cancelled the same run context the session's did; it no longer does,
	// and a client that showed "halted" for a workload still doing work
	// through the rest of its roster would be describing the wrong
	// session. Each specialist reports its own state in Scopes.
	//
	// Precluded rather than crossed, matching the door: a session sitting
	// exactly on its cap has crossed nothing and cannot make another call,
	// and answering "healthy" to an operator whose next turn will 409 is
	// the report this field exists to avoid.
	blocking := m.PrecludedSession()
	cc.Tripped = len(blocking) > 0
	// The crossed reason when there is one, because it names money that
	// was actually spent; the preclusion's arithmetic otherwise.
	cc.Reason = joinReasons(tripsForScope(trips, ""))
	if cc.Reason == "" {
		cc.Reason = joinReasons(blocking)
	}
	// No flag sits between the accumulator and the ceiling, so a reset
	// that adds nothing changes nothing: whenever it is tripped, a bare
	// reset would re-trip.
	cc.WouldRetrip = cc.Tripped

	fired := g.wds.alerts(sid)
	mode := g.wds.mode
	halted, haltReason := g.wds.halted(sid)
	// Reason answers "what do I do?", so the halt text wins when there
	// is one: it names the signal AND the reset endpoint, where the
	// last alert only names the behavior.
	reason := fired.last
	if halted {
		reason = haltReason
	}
	return attach.GuardrailInfo{
		Watchdog: attach.WatchdogInfo{
			Mode: string(mode),
			// Advisory is the operator's real question — "will this
			// thing stop my agent?" — and under enforce the answer is
			// yes whether or not it has yet.
			Advisory: !mode.Enforces(),
			Tripped:  halted,
			Alerts:   fired.count,
			Reason:   reason,
		},
		CostCeiling: cc,
		Halted:      cc.Tripped || halted,
	}
}

// tripsForScope filters a whole-meter trip list down to one ceiling
// holder's — scope "" being the session's own. Since W10.3 the meter's
// answers span the roster and almost every caller here wants one row of
// it: a reset targeting the session must not be judged by a specialist's
// ceiling, and a specialist's must not be judged by the session's.
func tripsForScope(trips []budget.Trip, scope string) []budget.Trip {
	var out []budget.Trip
	for _, t := range trips {
		if t.Scope == scope {
			out = append(out, t)
		}
	}
	return out
}

// exhaustedScopes names the specialists that can admit no further call,
// skipping the one a reset is already aimed at. It is what turns "your
// grant landed, and nothing here was tripped" from a true statement into
// a useful one: the operator raising a session budget to unstick a
// workload is usually looking at a spent specialist, and since W10.3 that
// is no longer the same thing.
func exhaustedScopes(m *budget.Meter, except string) []string {
	var out []string
	seen := map[string]bool{except: true}
	for _, t := range m.PrecludedAll() {
		if t.Scope == "" || seen[t.Scope] {
			continue
		}
		seen[t.Scope] = true
		out = append(out, t.Scope)
	}
	return out
}

// scopes projects each specialist that carries its own ceilings.
//
// Since W10.3 this is the part of the answer that carries the news: a
// spent specialist no longer halts the session, so if it is not visible
// here it is not visible anywhere, and the workload quietly loses a path
// it used to have.
func (g *guardrailView) scopes(m *budget.Meter, trips []budget.Trip) []attach.ScopeCeilingInfo {
	names := m.ScopeNames()
	out := make([]attach.ScopeCeilingInfo, 0, len(names))
	for _, name := range names {
		lim, _ := m.ScopeLimits(name)
		tokens, cost, calls, _ := m.ScopeSnapshot(name)
		own := tripsForScope(trips, name)
		// Tripped means "this specialist cannot be dispatched again",
		// which is one call earlier than "it went over" — the same
		// distinction the session's Tripped draws, for the same reason.
		// Precluded reports the session's ceilings alongside the scope's;
		// only the scope's belong to this row.
		blocked := tripsForScope(m.Precluded(name), name)
		reason := joinReasons(own)
		if reason == "" {
			reason = joinReasons(blocked)
		}
		out = append(out, attach.ScopeCeilingInfo{
			Name:       name,
			MaxCostUSD: lim.MaxCostUSD,
			CostUSD:    cost,
			MaxTokens:  lim.MaxTokens,
			Tokens:     tokens,
			MaxTurns:   lim.MaxTurns,
			Turns:      calls,
			Tripped:    len(blocked) > 0,
			Reason:     reason,
		})
	}
	return out
}

// reset services POST /sessions/{sid}/guardrails/reset.
//
// Order matters: the retrip check runs before anything is mutated, so
// a refused reset leaves the session exactly as it found it. Clearing
// the watchdog first and then 409-ing on the budget would report "no
// change" while having made one.
func (g *guardrailView) reset(sid string, req attach.GuardrailResetRequest) (attach.GuardrailResetResponse, error) {
	// Before the retrip check reads any state: a reset issued against a
	// restarted daemon has to see the halt it is clearing, or it reports
	// "nothing was tripped" and leaves the durable trip in place for the
	// next turn to restore. The spend half matters more here than
	// anywhere — the retrip check is arithmetic against the accumulator,
	// and run against an empty one it would clear a session that is still
	// $5 over and hand the operator a 200 saying so.
	g.wds.restore(context.Background(), sid)
	g.meters.restore(context.Background(), sid)

	target := req.Guardrail
	if target == "" {
		target = attach.GuardrailAll
	}
	wantWatchdog := target == attach.GuardrailWatchdog || target == attach.GuardrailAll
	wantCost := target == attach.GuardrailCostCeiling || target == attach.GuardrailAll

	add := budget.Limits{
		MaxCostUSD: req.AdditionalBudgetUSD,
		MaxTokens:  req.AdditionalTokens,
		MaxTurns:   req.AdditionalTurns,
	}
	granting := add != (budget.Limits{})
	if granting && !wantCost {
		// Rejected rather than dropped: an operator who thinks they
		// bought runway and didn't will find out on the next turn,
		// which is the worst possible moment.
		return attach.GuardrailResetResponse{}, fmt.Errorf(
			"additional budget has no meaning for the %q guardrail; target %q or %q",
			target, attach.GuardrailCostCeiling, attach.GuardrailAll)
	}

	m := g.meters.meter(sid)
	if req.Scope != "" {
		if _, ok := m.ScopeLimits(req.Scope); !ok {
			// Caught before the retrip check so a typo'd specialist
			// name reads as "no such scope" rather than as a budget
			// that refuses to clear.
			return attach.GuardrailResetResponse{}, fmt.Errorf(
				"scope %q declares no budget on this workload (scopes: %v)", req.Scope, m.ScopeNames())
		}
	}
	resp := attach.GuardrailResetResponse{Reset: []string{}}

	var costTrips []budget.Trip
	if wantCost {
		// What the grant is being asked to clear, and only that (W10.3).
		// Both of these read the whole meter while any crossed ceiling
		// wedged the session; now that a spent specialist does not,
		// judging a workload-level grant by a specialist's ceiling would
		// hold the session hostage to one that is not stopping it — and
		// reporting cost_ceiling "cleared" off the back of a trip the
		// grant never touched is the same error told the other way round.
		//
		// Precluded rather than crossed for the reason the door is: a
		// grant that lands the target exactly on its new cap has cleared
		// nothing, and a 200 that buys nothing is what this check exists
		// to prevent.
		costTrips = tripsForScope(m.Trips(), req.Scope)
		remaining := tripsForScope(m.PrecludedAfter(req.Scope, add), req.Scope)
		if len(remaining) > 0 {
			// Nothing has been mutated at this point, so the operator
			// can re-issue with a bigger number and lose nothing.
			return attach.GuardrailResetResponse{
					Reset:      []string{},
					Guardrails: g.info(sid),
				}, fmt.Errorf("%w: still over after the requested grant: %s",
					attach.ErrGuardrailRetrip, joinReasons(remaining))
		}
	}

	if wantWatchdog {
		fired := g.wds.alerts(sid)
		halted, _ := g.wds.halted(sid)
		g.wds.reset(sid)
		if fired.count > 0 || halted {
			resp.Reset = append(resp.Reset, attach.GuardrailWatchdog)
		}
	}

	if wantCost && granting {
		before := scopeLimits(m, req.Scope)
		after, err := m.Grant(req.Scope, add)
		if err != nil {
			return attach.GuardrailResetResponse{}, err
		}
		resp.BudgetAddedUSD = after.MaxCostUSD - before.MaxCostUSD
		resp.TokensAdded = after.MaxTokens - before.MaxTokens
		resp.TurnsAdded = after.MaxTurns - before.MaxTurns
	}
	if wantCost && len(costTrips) > 0 {
		resp.Reset = append(resp.Reset, attach.GuardrailCostCeiling)
	}

	resp.Guardrails = g.info(sid)
	resp.Message = resetMessage(resp, req.Scope, exhaustedScopes(m, req.Scope))

	// The audit trail is this log line and a row in mast's own
	// agent_guardrail_log — deliberately not an ADK session event. An
	// out-of-band append to the session stales the handle a running turn
	// holds and trips ADK's optimistic-concurrency check, the
	// write-lease constraint that already forced attachadapter to defer
	// its interrupt audit to between turns; a reset arrives from an
	// operator mid-incident, which is exactly when a turn is running. A
	// table mast owns has no such contention, so the row can be written
	// inline, here, where the decision is made.
	g.wds.recordReset(sid, target, req.Caller, req.Scope, resp)
	g.logger.Warn("guardrail reset",
		"session", sid,
		"caller", req.Caller,
		"guardrail", target,
		"scope", req.Scope,
		"cleared", strings.Join(resp.Reset, ","),
		"added_usd", fmt.Sprintf("%.4f", resp.BudgetAddedUSD),
		"added_tokens", resp.TokensAdded,
		"added_turns", resp.TurnsAdded,
	)
	return resp, nil
}

// scopeLimits reads the ceilings a grant is about to raise. An unknown
// scope reads as zero, and Grant reports the error.
func scopeLimits(m *budget.Meter, scope string) budget.Limits {
	if scope == "" {
		return m.SessionLimits()
	}
	l, _ := m.ScopeLimits(scope)
	return l
}

// joinReasons renders trips as one operator-facing line, scope-tagged
// so "$0.05 > cap $0.04" says whose ceiling that was.
func joinReasons(trips []budget.Trip) string {
	if len(trips) == 0 {
		return ""
	}
	parts := make([]string, 0, len(trips))
	for _, t := range trips {
		if t.Scope == "" {
			parts = append(parts, t.Reason)
			continue
		}
		parts = append(parts, fmt.Sprintf("specialist %q: %s", t.Scope, t.Reason))
	}
	return strings.Join(parts, "; ")
}

// resetMessage is the one-liner a client renders. It leads with what
// changed, because "nothing was tripped" and "you now have $10 more"
// are the two answers an operator is reading for.
//
// stillOut names specialists this reset did not reach. Since W10.3 a
// spent specialist does not trip the session, so the honest answer to a
// session grant can be "nothing was tripped" while the workload is short
// a path — true, and by itself the most misleading thing this endpoint
// could say. The tail turns it into the next command to run.
func resetMessage(resp attach.GuardrailResetResponse, scope string, stillOut []string) string {
	return resetSummary(resp, scope) + exhaustedNote(stillOut)
}

// exhaustedNote renders the tail. Names rather than a count: "1
// specialist" sends the operator back to GET /guardrails to find out
// which, and the name is the argument the follow-up reset needs.
func exhaustedNote(stillOut []string) string {
	if len(stillOut) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(stillOut))
	for _, s := range stillOut {
		quoted = append(quoted, fmt.Sprintf("%q", s))
	}
	return fmt.Sprintf("; %s can admit no further call (raise with scope=%q)",
		"specialist "+strings.Join(quoted, ", "), stillOut[0])
}

func resetSummary(resp attach.GuardrailResetResponse, scope string) string {
	where := "session"
	if scope != "" {
		where = fmt.Sprintf("specialist %q", scope)
	}
	var raised []string
	if resp.BudgetAddedUSD > 0 {
		raised = append(raised, fmt.Sprintf("+$%.4f", resp.BudgetAddedUSD))
	}
	if resp.TokensAdded > 0 {
		raised = append(raised, fmt.Sprintf("+%d tokens", resp.TokensAdded))
	}
	if resp.TurnsAdded > 0 {
		raised = append(raised, fmt.Sprintf("+%d turns", resp.TurnsAdded))
	}
	switch {
	case len(resp.Reset) == 0 && len(raised) == 0:
		return "nothing was tripped; no ceilings changed"
	case len(raised) == 0:
		return "cleared " + strings.Join(resp.Reset, ", ")
	case len(resp.Reset) == 0:
		return fmt.Sprintf("raised the %s budget (%s); nothing was tripped",
			where, strings.Join(raised, ", "))
	default:
		return fmt.Sprintf("cleared %s; raised the %s budget (%s)",
			strings.Join(resp.Reset, ", "), where, strings.Join(raised, ", "))
	}
}
