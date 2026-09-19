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

// The two seams a durable spend ledger needs, and nothing else (#175).
//
// A ceiling that a restart resets is not a ceiling. This package meters
// in memory by design — the arithmetic is the same whether the caller is
// a daemon, a library embed, or an eval rig, and none of them should
// have to hand it a database. So the durability lives outside: a writer
// (Config.OnSpend) that sees each priced call as it is folded, and a
// reader (Meter.Restore) that seeds a fresh meter with what a previous
// process already spent. What sits between them — a table, a file, a
// remote counter — is the caller's problem. cmd/mast's is
// eventlog.SpendStore.
//
// Why a per-call hook rather than "persist the accumulator":
//
// The accumulator is derived state. Checkpointing it means choosing a
// moment to write it and then reconciling that moment against a
// transcript that may have advanced past it — the classic gap between
// "what the meter says" and "what actually got billed". A ledger of the
// calls themselves has no such gap: it is written at the same
// granularity the accumulator moves, so the only thing a crash can lose
// is a call whose row had not landed yet. That loss is bounded by one
// model call and is always an undercount, never a phantom charge.
//
// Why not replay the session's events instead (#175's other option):
//
//   - ADK's database session service does not persist ModelVersion
//     (session/database storage_session.go carries UsageMetadata and
//     Author but has no column for it, verified against adk/v2 v2.2.0).
//     Replayed events therefore miss the catalog on every lookup and
//     fall through to the flat RatePer1K — the approximation this
//     package's own docs measured at 5.9x on a real cache-warm session.
//     A restored ceiling that wrong is worse than none, because it looks
//     right.
//   - Replay prices history at today's rates. The catalog moves weekly
//     (dev/regen-builtin-pricing), so a rate change would retroactively
//     rewrite what a session already spent. Spend is a claim about money
//     that has left the account; it is a fact of the moment the call was
//     made, and the ledger records it then.
//   - Replay's cost grows with the transcript, and the sessions that
//     survive a restart are the long ones.

package budget

import (
	"errors"
	"fmt"
	"time"
)

// ErrRestored is returned by a second Restore on the same meter. Prior
// spend is additive, so folding it twice doubles it — and the sessions
// this matters for are the ones already near a ceiling, where the double
// count is the difference between running and refused.
var ErrRestored = errors.New("budget: meter has already been restored")

// Spend is one metered model call's contribution, handed to
// Config.OnSpend after the meter has folded it.
//
// Author is the event's author verbatim, not the scope it resolved to.
// The distinction matters across a restart: a roster edit can add or
// remove a specialist's scope between processes, and a ledger that
// recorded "session" for an author the old config did not scope could
// never be re-attributed. Recording the author lets Restore attribute
// against whatever scopes the *current* config carries, which is exactly
// what a live Observe does.
//
// # The breakdown half
//
// The first four fields are what a durable ledger needs. The rest are
// what a usage report needs, and they are here rather than derived by a
// second reader of the event because deriving them a second time is how
// the two disagree: the split of a prompt into cached, written and fresh
// is not a copy of the provider's counters but the meter's own reading
// of them (see callOf and fitBucket), and it is the reading the money
// was computed from. A report keyed off anything else is a breakdown of
// a different call than the one that was billed.
//
// Everything below the line is reported, not priced, except Call and
// CostUSDUncachedReference. A consumer that only wants the ledger keeps
// ignoring them — the four original fields have not moved.
type Spend struct {
	Author   string
	Tokens   int64
	CostUSD  float64
	Unpriced bool

	// At is the event's own timestamp, which is when the call landed
	// rather than when the hook ran. Zero for an event a producer left
	// unstamped; a consumer that needs an ordering has the arrival order
	// of the hook itself.
	At time.Time

	// Model is the id the price was resolved against — see priceOf. It
	// is empty only when the event named no model and the meter was
	// configured with none, which on a streaming Gemini run is the
	// common case for the first half of that pair (Limits.Model exists
	// for exactly that).
	Model string

	// Call is the billable split of this call: the three mutually
	// exclusive input buckets and the output total. Output includes
	// thinking, because thinking bills at the output rate.
	Call Call

	// ThoughtsTokens is the thinking subset of Call.OutputTokens, and
	// ToolUseTokens the tool-definition subset of the prompt. Both are
	// breakdowns of a bucket already counted above, never additions to
	// it — adding either to a total double-counts it.
	//
	// Both come from genai's usage metadata rather than from the
	// provider sidecar. The sidecar's equivalents (usage.Detail's
	// ReasoningTokens and ToolUseTokens) are the same two Gemini
	// counters, and the sidecar exists for the buckets genai has no
	// field for, which these are not.
	ThoughtsTokens int64
	ToolUseTokens  int64

	// CostUSDUncachedReference is what this call would have cost on a
	// backend with no prompt cache at all: every prompt token fresh, at
	// the plain input rate.
	//
	// Usually above CostUSD, and the gap is what the cache saved — but
	// deliberately not floored there. A turn that WRITES a cache entry
	// costs more than one that does not (Anthropic's 5-minute TTL bills
	// writes at 1.25x fresh input), so on a warming turn this figure is
	// below the cost, and a workload that warms caches it never reuses
	// should be able to see that it is paying for nothing.
	//
	// Equal to CostUSD when no saving can be computed rather than when
	// none was made: a call the rate card could not price falls back to
	// the flat rate, which has no notion of a cache.
	CostUSDUncachedReference float64
}

// Totals is one accumulator's durable form — the exported shape of the
// package's internal usage counter.
type Totals struct {
	Tokens  int64
	CostUSD float64
	Calls   int
}

// Prior is what a session spent before this process started.
//
// Session is the whole session's total, including every call that
// ByAuthor also accounts for; the two are projections of the same rows
// and a producer must not let them disagree. ByAuthor is keyed by
// Spend.Author, and Restore ignores an author the meter carries no scope
// for — the same rule Observe applies to a live event.
type Prior struct {
	Session  Totals
	ByAuthor map[string]Totals
	Unpriced int
}

// IsZero reports whether there is nothing to restore, which is the
// common case: most sessions are new.
func (p Prior) IsZero() bool {
	return p.Session == (Totals{}) && len(p.ByAuthor) == 0 && p.Unpriced == 0
}

// Restore seeds the meter with spend from before this process.
//
// It adds rather than assigns, and it deliberately does not check the
// ceilings it may have just crossed. Both follow from where enforcement
// lives: there is no trip flag (see trips.go), so a restored meter that
// is already over its cap reports that through Trips() and stops the
// next call through Observe, by the same comparison a meter that never
// restarted would use. Restoring is not an enforcement point; it is the
// accumulator catching up with the facts.
//
// Calling it twice is an error, not a no-op — see ErrRestored. Calling
// it on a meter that has already observed events is allowed: a caller
// whose first read failed and who retried a turn later has real spend in
// this process and real spend in the ledger, and they sum.
func (m *Meter) Restore(p Prior) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.restored {
		return ErrRestored
	}
	m.restored = true

	m.total.tokens += p.Session.Tokens
	m.total.cost += p.Session.CostUSD
	m.total.calls += p.Session.Calls
	m.unpriced += p.Unpriced

	for author, t := range p.ByAuthor {
		u, ok := m.spent[author]
		if !ok {
			// No scope for this author under the current config. Its
			// tokens are already in the session total above; there is no
			// ceiling of its own left to meter them against.
			continue
		}
		u.tokens += t.Tokens
		u.cost += t.CostUSD
		u.calls += t.Calls
	}
	return nil
}

// Restored reports whether prior spend has been folded into this meter.
func (m *Meter) Restored() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restored
}

// String renders totals for a log line.
func (t Totals) String() string {
	return fmt.Sprintf("$%.4f over %d calls (%d tokens)", t.CostUSD, t.Calls, t.Tokens)
}
