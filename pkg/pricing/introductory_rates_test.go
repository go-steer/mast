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

package pricing_test

import (
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/pricing"
)

// A rate can be current in its source and still be wrong.
//
// Rates.UpdatedAt answers "when did we last read this from LiteLLM", and
// /pricing surfaces it so operators can spot a row that has gone stale.
// That is the right signal for the failure it was built for (#259: a
// hand-authored table drifting silently) and the wrong one for a
// promotional rate with a known end date. The weekly regen re-reads
// LiteLLM, LiteLLM keeps returning the promotional number until it
// updates its own table, and each run stamps a fresh UpdatedAt on an
// unchanged value — so the freshness signal reports maximum confidence in
// a number that has become false. Same shape as #174: the guard measures
// a property adjacent to the one that matters.
//
// It cannot be fixed upstream either. LiteLLM's catalog carries no expiry
// metadata on any row — 1.7 MB of costs and capability flags, and no
// field that could say "this price ends on the 31st" (checked
// 2026-08-19). If the fact is going to exist anywhere, it exists here.
//
// So: a dated assertion. Crude, and deliberately so — it costs one table
// entry, it cannot be forgotten, and it converts a date in someone's head
// into a build failure.
//
// This runs inside pricing-regen.yml as well as ordinary CI — the
// workflow's verify step calls dev/ci/presubmits/test.sh before opening
// the auto-PR — so an expiry that LiteLLM has not caught up with stops
// the regen rather than being rubber-stamped by it.
//
// # The 2026-08-20 enumeration (#188)
//
// #188 asked the question this file could not answer from inside: are
// there OTHER rows on a promotional price? Nobody had checked. Every
// Anthropic and Gemini row was then walked against the providers'
// published pricing, and the answer was three, none of which behaved the
// way the one known entry was written to expect.
//
//   - claude-sonnet-5's increase to $3/$15 was CANCELLED. Anthropic made
//     the introductory number the standard one, so this test would have
//     gone red on 2026-09-01 over a rate that is correct. `resolved`
//     exists for that outcome — see the field.
//   - gemini-3.7-flash and gemini-3.6-flash are BOTH on $0.75/$3.75
//     through 2026-12-31, doubling to $1.50/$7.50 on 2027-01-01, with
//     the cache-read rate doubling alongside. Nothing in the repo
//     recorded either. gemini-3.7-flash is the gemini/vertex frontier
//     default.
//
// The instructive one is gemini-3.6-flash, because mast had already seen
// its rate move and drawn the wrong conclusion from it. The 2026-08-19
// regen (#184) halved that row, the PR body said only "Automated
// regeneration", and the change was read as a permanent price cut —
// pkg/taskclass's frontier comment was written on that reading and was
// wrong within the day. It was not a cut. It was Google moving the older
// model onto the newer one's introductory rate, and the January standard
// price is exactly what 3.6-flash cost at launch. A regen that had said
// "gemini-3.6-flash: $1.50/$7.50 -> $0.75/$3.75" would have prompted the
// question. #188's other half is that report.
//
// So the table's own history is: one promotional rate that never
// expired, and two that will. Both directions have to be expressible,
// and neither is derivable from the source of truth.
type introductoryRate struct {
	model string

	// notBefore is when this assertion starts biting: the first instant
	// at which the introductory rate still being present is a defect.
	// Set it to the day *after* the published expiry. The extra day is
	// not politeness, it is avoiding a false red — "expires 2026-08-31"
	// does not say which hour of the 31st, and a spurious failure on a
	// guard like this one teaches people to delete it.
	notBefore time.Time

	// introInput/introOutput are the promotional per-MTok rates. The
	// assertion fires only while the table still carries *these* exact
	// numbers; any other value means the rate has moved and this entry
	// has done its job.
	introInput, introOutput float64

	// what to expect instead, for the failure message.
	standard string

	why string

	// resolved, when non-empty, records that the scheduled increase is
	// not going to happen — the provider made the promotional rate
	// permanent, or moved the date. The entry stays rather than being
	// deleted, and inverts: the assertion now pins the rate AT the
	// former promotional number.
	//
	// This exit matters as much as the expiry one. A dated assertion
	// whose only outcomes are "fires" and "gets deleted" cannot record
	// that a price was provisional and then became standard, and that
	// is the outcome this table's first and only entry actually had.
	// Deleting it would leave claude-sonnet-5 sitting at $2 next to
	// claude-sonnet-4-6 at $3 with nothing in the repo explaining why —
	// which is a smaller version of the gap the file was written to
	// close. Text goes in the failure message so whoever trips the pin
	// knows what was already checked.
	resolved string
}

var introductoryRates = []introductoryRate{
	{
		model:       "claude-sonnet-5",
		notBefore:   time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		introInput:  2,
		introOutput: 10,
		standard:    "$3 in / $15 out per MTok",
		why: "claude-sonnet-5 is the anthropic mid-tier default (pkg/modeltier), " +
			"so every tier:mid specialist on Anthropic prices through this row — " +
			"including max_cost_usd, which means an understated rate lets a " +
			"workload spend past its ceiling before the guardrail trips",
		resolved: "checked 2026-08-20 against platform.claude.com/docs/en/about-claude/pricing: " +
			"\"The $2/$10 per million input/output token pricing for Claude Sonnet 5, " +
			"announced at launch as introductory pricing through August 31, 2026, is now " +
			"the standard price. The previously scheduled increase to $3/$15 per million " +
			"input/output tokens on September 1, 2026 will not occur.\" So $2/$10 is the " +
			"standard rate for this model and the builtin table is correct as it stands",
	},
	{
		model:       "gemini-3.7-flash",
		notBefore:   time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC),
		introInput:  0.75,
		introOutput: 3.75,
		standard:    "$1.50 in / $7.50 out per MTok (and CachedInputPerMTok 0.075 -> 0.15)",
		why: "gemini-3.7-flash is the gemini/vertex FRONTIER default (ModelForTier), so " +
			"it is the most expensive model mast picks on its own and the one every " +
			"unqualified frontier workload prices through. The scheduled move is a " +
			"doubling, not a nudge: a max_cost_usd sized against $0.75/$3.75 buys half " +
			"the tokens it is budgeted for from the moment the rate changes, and buys " +
			"them without the ceiling noticing",
	},
	{
		model:       "gemini-3.6-flash",
		notBefore:   time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC),
		introInput:  0.75,
		introOutput: 3.75,
		standard:    "$1.50 in / $7.50 out per MTok (and CachedInputPerMTok 0.075 -> 0.15)",
		why: "not a mast default, but it is the row #184 moved ($1.50/$7.50 -> $0.75/$3.75) " +
			"and the move was Google putting the OLDER model onto the newer one's " +
			"introductory rate rather than a permanent cut — the January standard price " +
			"is exactly what 3.6-flash cost at its own launch. An operator who pinned it " +
			"reads the same doubling",
	},
}

// TestIntroductoryRatesHaveNotOutlivedTheirExpiry fails once a known
// promotional rate is past its end date and the builtin table still
// carries it.
//
// The test is time-dependent, which is normally a defect and here is the
// entire mechanism. It has four outcomes:
//
//   - resolved (the increase was cancelled): the assertion inverts and
//     pins the rate at the former promotional number. Time stops
//     mattering.
//   - before notBefore: nothing to say, the rate is correct.
//   - after notBefore, rate moved: the entry is spent; the test says so
//     and passes. Delete the entry.
//   - after notBefore, rate unchanged: fail. Either LiteLLM has not
//     caught up (override the row, or wait for the next regen and accept
//     the exposure knowingly) or the expiry was wrong (fix the entry).
func TestIntroductoryRatesHaveNotOutlivedTheirExpiry(t *testing.T) {
	builtin := pricing.Builtin()
	now := time.Now().UTC()

	for _, ir := range introductoryRates {
		t.Run(ir.model, func(t *testing.T) {
			r, ok := builtin[ir.model]
			if !ok {
				// The row left the table entirely — eligible() no longer
				// matches it, or LiteLLM dropped it. Nothing to assert
				// against, and the entry here is now noise.
				t.Skipf("%s is no longer in pricing.Builtin(); drop its introductoryRates entry", ir.model)
			}

			stillIntroductory := r.InputPerMTok == ir.introInput && r.OutputPerMTok == ir.introOutput

			// A cancelled increase inverts the assertion: the former
			// promotional number IS the rate, so drift away from it is
			// what's suspicious now. notBefore no longer applies —
			// leaving it in force would have turned a correct table red
			// on the date the increase was scheduled and then didn't
			// happen, which is a guard failing in the direction that
			// gets guards deleted.
			if ir.resolved != "" {
				if !stillIntroductory {
					t.Errorf("%s prices at $%g in / $%g out, but this entry records $%g / $%g "+
						"as permanent.\nWhat was checked: %s.\n"+
						"Either the provider repriced again — confirm on the public pricing page "+
						"and update or drop this entry — or the builtin table is wrong.",
						ir.model, r.InputPerMTok, r.OutputPerMTok, ir.introInput, ir.introOutput,
						ir.resolved)
				}
				return
			}

			if now.Before(ir.notBefore) {
				if !stillIntroductory {
					t.Logf("%s moved off its introductory rate early (now $%g in / $%g out); "+
						"the introductoryRates entry can be dropped",
						ir.model, r.InputPerMTok, r.OutputPerMTok)
				}
				return
			}

			if !stillIntroductory {
				t.Logf("%s is past %s and off its introductory rate ($%g in / $%g out); "+
					"this entry has done its job and can be dropped",
					ir.model, ir.notBefore.Format("2006-01-02"), r.InputPerMTok, r.OutputPerMTok)
				return
			}

			t.Errorf("%s still prices at its introductory rate ($%g in / $%g out) on %s, "+
				"past the %s expiry — expected %s.\n"+
				"UpdatedAt says %s, which is why nothing else caught this: the row is "+
				"freshly read from LiteLLM and still wrong.\n"+
				"Why it matters: %s.\n"+
				"To resolve: confirm the published rate, then either wait for LiteLLM "+
				"(next regen is the following Monday) or override the row via "+
				"pricing.json / .agents/config.json model.pricing. Update this entry once it moves.",
				ir.model, r.InputPerMTok, r.OutputPerMTok,
				now.Format("2006-01-02"), ir.notBefore.Format("2006-01-02"), ir.standard,
				r.UpdatedAt.Format("2006-01-02"), ir.why)
		})
	}
}
