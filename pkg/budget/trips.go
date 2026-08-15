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

// Reporting a crossed ceiling, and the only thing that can clear one.
//
// The Meter has no trip flag, and deliberately: enforcement is derived
// on every Observe from accumulator-vs-ceiling, so there is no state to
// unset. The consequence is the whole reason this file exists — once a
// session's spend is past its cap, every subsequent turn crosses it
// again on its first priced event, and the session is wedged for the
// daemon's lifetime. "Reset" therefore cannot mean clearing a flag; it
// means raising the ceiling (Grant), which is also the only reading
// that leaves the session's reported spend agreeing with what it
// actually spent.
//
// Zeroing the accumulator was the alternative, and it is worse: /usage,
// the eventlog-derived cost, and the ceiling check would then disagree
// about the same dollars, and an operator reviewing the incident later
// would find a session that spent $40 reporting $10.

package budget

import (
	"fmt"
	"sort"
)

// Budget dimensions, as reported on Trip.Dimension. One per ceiling in
// Limits; a session can be past more than one at once.
const (
	DimensionTurns   = "turns"
	DimensionTokens  = "tokens"
	DimensionCostUSD = "cost_usd"
)

// Trip is one crossed ceiling: which accumulator crossed which bound,
// and the operator-facing arithmetic behind it.
type Trip struct {
	// Scope is the specialist whose own ceiling was crossed, or "" for
	// the session's.
	Scope string
	// Dimension is which bound was crossed (turns / tokens / cost_usd).
	Dimension string
	// Reason is the same detail string the enforcement error carries,
	// e.g. "$0.0612 > cap $0.0500 (30600 tokens over 4 calls)".
	Reason string
}

// Trips reports every ceiling the meter is currently past — the
// session's and each scope's. Empty means the next turn will not be
// stopped by this meter.
func (m *Meter) Trips() []Trip {
	return m.TripsAfter("", Limits{})
}

// TripsAfter reports the trips that would remain if scope's ceilings
// were raised by add — the check an operator reset runs before
// spending the operator's grant on a raise that provably would not
// clear anything.
//
// It reports the whole meter, not just scope, because any crossed
// ceiling wedges the session: raising the workload's cap while a
// specialist's own cap is still crossed buys nothing, and a 200 that
// buys nothing is what this reporting exists to prevent.
//
// scope "" targets the session. An unknown scope name raises nothing,
// which correctly reports the trips as unclearable by that request.
func (m *Meter) TripsAfter(scope string, add Limits) []Trip {
	m.mu.Lock()
	defer m.mu.Unlock()

	sessionLimits := m.limits
	if scope == "" {
		sessionLimits = raise(sessionLimits, add)
	}
	out := crossed("", sessionLimits, &m.total)

	for _, name := range m.sortedScopesLocked() {
		l := m.scopes[name]
		if name == scope {
			l = raise(l, add)
		}
		out = append(out, crossed(name, l, m.spent[name])...)
	}
	return out
}

// Grant raises scope's ceilings by add and returns the resulting
// limits. scope "" targets the session; an unknown scope name is an
// error rather than a silent no-op, since the caller believes it just
// bought runway.
//
// A dimension that was already unlimited stays unlimited: adding
// budget must never be able to *impose* a ceiling. Handing 5 turns to
// a session with no turn cap would otherwise cap it at 5 — a reset
// that halts the session it was called to unwedge.
func (m *Meter) Grant(scope string, add Limits) (Limits, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if scope == "" {
		m.limits = raise(m.limits, add)
		return m.limits, nil
	}
	l, ok := m.scopes[scope]
	if !ok {
		return Limits{}, fmt.Errorf("budget: no scope %q (this meter carries ceilings for %v)", scope, m.sortedScopesLocked())
	}
	l = raise(l, add)
	m.scopes[scope] = l
	return l, nil
}

// raise returns l with each already-bounded dimension extended by the
// matching field of add. Zero fields of add change nothing.
func raise(l Limits, add Limits) Limits {
	if l.MaxTurns > 0 && add.MaxTurns > 0 {
		l.MaxTurns += add.MaxTurns
	}
	if l.MaxTokens > 0 && add.MaxTokens > 0 {
		l.MaxTokens += add.MaxTokens
	}
	if l.MaxCostUSD > 0 && add.MaxCostUSD > 0 {
		l.MaxCostUSD += add.MaxCostUSD
	}
	return l
}

// SessionLimits returns the session's ceilings as they stand now,
// including any raised by Grant.
func (m *Meter) SessionLimits() Limits {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limits
}

// ScopeLimits returns one scope's ceilings. ok is false for an agent
// the meter carries no scope for, matching ScopeSnapshot.
func (m *Meter) ScopeLimits(name string) (Limits, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.scopes[name]
	return l, ok
}

// ScopeNames lists the scoped agents this meter meters, sorted, so a
// projection over them is stable between reads.
func (m *Meter) ScopeNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sortedScopesLocked()
}

// sortedScopesLocked lists scope names in a stable order. Caller holds
// m.mu.
func (m *Meter) sortedScopesLocked() []string {
	if len(m.scopes) == 0 {
		return nil
	}
	out := make([]string, 0, len(m.scopes))
	for name := range m.scopes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
