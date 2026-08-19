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
// into a build failure. #188 tracks the general version (a
// provisional_until field on Rates, surfaced through /pricing, with the
// regen refusing to re-stamp an expired row); this is the instance.
//
// This runs inside pricing-regen.yml as well as ordinary CI — the
// workflow's verify step calls dev/ci/presubmits/test.sh before opening
// the auto-PR — so an expiry that LiteLLM has not caught up with stops
// the regen rather than being rubber-stamped by it.
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
	},
}

// TestIntroductoryRatesHaveNotOutlivedTheirExpiry fails once a known
// promotional rate is past its end date and the builtin table still
// carries it.
//
// The test is time-dependent, which is normally a defect and here is the
// entire mechanism. It has three outcomes:
//
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
