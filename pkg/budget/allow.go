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

// Asking the meter before the call instead of after it.
//
// Observe is the ledger: it folds a call that already happened, and a
// call that already happened cost what it cost whether or not it was the
// one that crossed the ceiling. That is the right shape for accounting
// and the wrong shape for enforcement, because by the time it reports
// ErrExceeded the money is gone. A $5 cap enforced only by Observe is a
// $5 cap plus one call, and on a frontier model with a large context
// that overshoot is not rounding.
//
// Allow is the other half: the same comparisons, asked before the call
// is issued, answering a narrower question. Observe asks "has a ceiling
// been crossed"; Allow asks "can this ceiling still be respected". They
// are deliberately not the same predicate, and the difference is where
// the value is.
//
// # What Allow will and will not claim
//
// Allow refuses only when the meter can *prove*, arithmetically, that
// the call it is being asked about cannot complete without crossing a
// ceiling. It never estimates the size of the next call. A pre-call
// check built on a guess at what a model is about to return would refuse
// affordable work on a bad guess and permit unaffordable work on a
// worse one, and the operator could not tell which had happened.
//
// The proof exists for each dimension:
//
//   - turns: exact. A call is one call, so calls+1 > MaxTurns is not a
//     projection, it is arithmetic. This is the dimension where Observe
//     is not merely late but structurally wrong — it can only ever
//     report the cap crossed by the call that crossed it, so a workload
//     capped at 3 turns has always made 4.
//   - tokens: tokens >= MaxTokens. Every real call adds tokens, so the
//     next one must cross.
//   - cost: cost >= MaxCostUSD, on the same reasoning.
//
// The >= for tokens and cost is where Allow is sharper than crossed's
// >, and the gap is exactly one call: at cost == cap the ceiling is met
// and not yet crossed, so Observe would permit one more call and then
// report the overshoot. Allow declines to spend a call establishing
// something it can already derive.
//
// A dimension with no ceiling proves nothing and refuses nothing, so an
// unmetered agent under an unlimited session is never refused — Allow
// costs a mutex and an integer compare on the common path.

package budget

import (
	"errors"
	"fmt"
)

// ErrRefused is returned by Allow when a ceiling makes the requested
// call impossible. It is distinct from ErrExceeded on purpose: the two
// describe different events, and a caller that conflated them would
// report money spent that was not.
//
// ErrExceeded means a call happened and pushed the meter past a cap.
// ErrRefused means a call did not happen, because it could not have
// finished under one.
var ErrRefused = errors.New("budget refused the call")

// Allow reports whether author may make another model call now, or the
// reason it may not.
//
// It checks author's own scope first and then the session, matching the
// order fold reports a crossing in, so the refusal names the tighter of
// the two ceilings rather than whichever was tested first. An author
// the meter carries no scope for is checked against the session alone.
//
// A nil Meter allows everything. That is not a convenience: it is the
// statement "no budget was configured for this call", which is what a
// host says when the caller runs outside a metered session, and it is
// deliberately different from an empty Meter, which says "metered, with
// no ceilings".
func (m *Meter) Allow(author string) error {
	err := m.allow(author)
	if err != nil {
		m.noteRefusal(err)
	}
	return err
}

func (m *Meter) allow(author string) error {
	// The first precluding ceiling is the one reported. Precluded orders
	// the author's own scope ahead of the session's, which is the order
	// an operator wants: "this specialist is out" is more actionable than
	// "the workload is out" when both are true.
	p := m.Precluded(author)
	if len(p) == 0 {
		return nil
	}
	if t := p[0]; t.Scope != "" {
		return fmt.Errorf("%w: specialist %q: %s", ErrRefused, t.Scope, t.Reason)
	}
	return fmt.Errorf("%w: %s", ErrRefused, p[0].Reason)
}

func (m *Meter) noteRefusal(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refusals++
	if m.firstRefusal == nil {
		m.firstRefusal = err
	}
}

// Refusals reports how many calls Allow has refused and the reason it
// gave for the first of them.
//
// A refusal has to be legible somewhere, and the meter is the only place
// that sees every one: the refusal itself is a synthesized answer, so a
// turn that ended because its ceiling stopped it otherwise looks exactly
// like a turn that finished. A driver snapshots the count before a turn
// and compares after — that, rather than asking whether a ceiling is
// *now* precluding, because a run that finished its work and happened to
// land exactly on its cap did not stop for the budget and must not be
// reported as though it had.
//
// The first reason rather than the last: a fan-out refused ten times
// says the same thing ten times, and the first one is the ceiling that
// actually stopped the work.
func (m *Meter) Refusals() (n int, first error) {
	if m == nil {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refusals, m.firstRefusal
}

// PrecludedAll lists every ceiling on the meter — the session's and each
// scope's — that already makes another call impossible. It is Trips's
// pre-call counterpart in the same way Precluded is crossed's, and it
// reports the whole meter for the reason TripsAfter does: any precluded
// ceiling wedges the session, so a caller deciding whether a session can
// still run has to see all of them.
func (m *Meter) PrecludedAll() []Trip {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	out := precluded("", m.limits, &m.total)
	for _, name := range m.sortedScopesLocked() {
		out = append(out, precluded(name, m.scopes[name], m.spent[name])...)
	}
	return out
}

// Precluded lists every ceiling that already makes another call by
// author impossible — the pre-call counterpart of Trips, and reported in
// the same shape so an operator surface can render one the way it
// renders the other.
//
// Empty means the call may proceed. It does not mean the call will fit:
// nothing here knows how large it will be.
func (m *Meter) Precluded(author string) []Trip {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Trip
	if scope, ok := m.scopes[author]; ok {
		out = append(out, precluded(author, scope, m.spent[author])...)
	}
	return append(out, precluded("", m.limits, &m.total)...)
}

// precluded lists the ceilings in l that u cannot make another call
// under. It mirrors crossed one call into the future, and the two are
// held to that relationship by TestARefusedCallReallyWouldHaveCrossed
// rather than by having been written next to each other.
//
// Reasons are phrased in the present tense of a refusal ("would be the
// Nth call") rather than crossed's past tense of an overshoot, because
// the numbers a reader is being shown are about a call that has not
// happened. A refusal that reads like a report of spending invites the
// operator to look for money that was never taken.
func precluded(scope string, l Limits, u *usage) []Trip {
	if u == nil {
		u = &usage{}
	}
	var out []Trip
	if l.MaxTurns > 0 && u.calls+1 > l.MaxTurns {
		out = append(out, Trip{Scope: scope, Dimension: DimensionTurns,
			Reason: fmt.Sprintf("would be model call (turn) %d of a cap of %d", u.calls+1, l.MaxTurns)})
	}
	if l.MaxTokens > 0 && u.tokens >= l.MaxTokens {
		out = append(out, Trip{Scope: scope, Dimension: DimensionTokens,
			Reason: fmt.Sprintf("%d tokens already at cap %d; any further call exceeds it", u.tokens, l.MaxTokens)})
	}
	if l.MaxCostUSD > 0 && u.cost >= l.MaxCostUSD {
		out = append(out, Trip{Scope: scope, Dimension: DimensionCostUSD,
			Reason: fmt.Sprintf("$%.4f already at cap $%.4f (%d tokens over %d calls); any further call exceeds it",
				u.cost, l.MaxCostUSD, u.tokens, u.calls)})
	}
	return out
}
