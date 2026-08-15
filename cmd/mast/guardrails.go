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

// watchdogModeWarn is the only watchdog posture mast ships. The
// enforce / prompt / feedback modes in the design doc are deferred;
// until one lands the honest answer to "what will the watchdog do" is
// "log, and let the turn run".
const watchdogModeWarn = "warn"

// info renders GET /sessions/{sid}/guardrails.
//
// It resolves the session's meter through the pool, which mints one if
// the session hasn't run a turn yet. That is deliberate: the meter is
// where a pre-emptive grant has to land, so the read and the reset
// must be looking at the same object the next turn will meter against.
func (g *guardrailView) info(sid string) attach.GuardrailInfo {
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
	// Tripped covers the scopes too: a specialist's crossed ceiling
	// cancels the same run context the session's does, so a client
	// rendering only the session's numbers would show a halted session
	// with a comfortable-looking budget.
	cc.Tripped = len(trips) > 0
	cc.Reason = joinReasons(trips)
	// No flag sits between the accumulator and the ceiling, so a reset
	// that adds nothing changes nothing: whenever it is tripped, a bare
	// reset would re-trip.
	cc.WouldRetrip = cc.Tripped

	fired := g.wds.alerts(sid)
	return attach.GuardrailInfo{
		Watchdog: attach.WatchdogInfo{
			Mode:     watchdogModeWarn,
			Advisory: true,
			Tripped:  false,
			Alerts:   fired.count,
			Reason:   fired.last,
		},
		CostCeiling: cc,
		Halted:      cc.Tripped,
	}
}

// scopes projects each specialist that carries its own ceilings.
func (g *guardrailView) scopes(m *budget.Meter, trips []budget.Trip) []attach.ScopeCeilingInfo {
	names := m.ScopeNames()
	out := make([]attach.ScopeCeilingInfo, 0, len(names))
	for _, name := range names {
		lim, _ := m.ScopeLimits(name)
		tokens, cost, calls, _ := m.ScopeSnapshot(name)
		var own []budget.Trip
		for _, t := range trips {
			if t.Scope == name {
				own = append(own, t)
			}
		}
		out = append(out, attach.ScopeCeilingInfo{
			Name:       name,
			MaxCostUSD: lim.MaxCostUSD,
			CostUSD:    cost,
			MaxTokens:  lim.MaxTokens,
			Tokens:     tokens,
			MaxTurns:   lim.MaxTurns,
			Turns:      calls,
			Tripped:    len(own) > 0,
			Reason:     joinReasons(own),
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
		costTrips = m.Trips()
		if remaining := m.TripsAfter(req.Scope, add); len(remaining) > 0 {
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
		g.wds.reset(sid)
		if fired.count > 0 {
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
	resp.Message = resetMessage(resp, req.Scope)

	// The audit trail is this log line, not an eventlog row. An
	// out-of-band append while a turn holds the session handle stales
	// that handle and trips ADK's optimistic-concurrency check — the
	// write-lease constraint that already forced attachadapter to defer
	// its interrupt audit to between turns. A reset arrives from an
	// operator mid-incident, which is exactly when a turn is running.
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
func resetMessage(resp attach.GuardrailResetResponse, scope string) string {
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
