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

	"github.com/go-steer/mast/pkg/modeltier"
	"github.com/go-steer/mast/pkg/pricing"
)

// TestBuiltinModelsKnownToCompanionTables pins the invariant that the
// model tables move together.
//
// pricing.Builtin is generated from LiteLLM by dev/regen-builtin-pricing
// and holds every Gemini/Anthropic model this project will run, so it is
// the natural source of truth. The other two tables answer the same
// operator-typed id — and both fail OPEN rather than loudly:
//
//   - modeltier.Classify returns "" for an unknown model, which drops
//     the per-tier compaction threshold and leaves the small-tier
//     parent guard unable to reason about the model.
//   - the generated context-window table has no row, so a consumer
//     falls back to a 0 sentinel — and a compactor that reads 0 as
//     "don't compact" silently disables threshold-based compaction for
//     the whole session.
//
// Neither surfaces at startup; you find out when a long session dies on
// a provider context-length error. A weekly regen that newly qualifies a
// model, with the companions left behind, is therefore a live defect
// this test converts into a build failure — and it runs inside
// pricing-regen.yml before the auto-PR opens, so the bad state never
// reaches a branch.
//
// mast difference from the parent project: there is no pkg/usage port
// yet, so the window half checks pricing's own generated table rather
// than usage.ContextWindowSizeFor. When pkg/usage lands, point this at
// it — the generated table is what that lookup is meant to prefer.
//
// SCOPE: this checks one direction only — priced => classified+sized.
// The reverse (a model the tier/window tables match but pricing does
// not, so cost tracking degrades to a flat estimate and cost ceilings
// lose their per-model accuracy) cannot be enumerated here: the
// classifier is a substring matcher, not a list, so there is no set to
// walk. Upstream closes the one place it mattered — the /model picker
// — by deriving it from this table; mast ships no picker, so the
// exposure is limited to ids an operator types by hand.
func TestBuiltinModelsKnownToCompanionTables(t *testing.T) {
	for id := range pricing.Builtin() {
		t.Run(id, func(t *testing.T) {
			if tier := modeltier.Classify(id); tier == "" {
				t.Errorf("modeltier.Classify(%q) = \"\" (unclassified); "+
					"add a case to pkg/modeltier/modeltier.go", id)
			}
			if size, ok := pricing.BuiltinContextWindow(id); !ok || size == 0 {
				t.Errorf("pricing.BuiltinContextWindow(%q) = %d, ok=%v "+
					"(unknown window disables threshold-based compaction); "+
					"regenerate with dev/regen-builtin-pricing", id, size, ok)
			}
		})
	}
}

// TestBuiltinTablesHaveMatchingKeys pins the two generated maps to each
// other. They are emitted from one pass over one filtered slice, so
// they can only diverge if someone hand-edits the DO-NOT-EDIT file —
// and a rate row with no window row would silently drop that model back
// onto usage's coarse substring fallback, which is the exact failure
// the window table was generated to end.
func TestBuiltinTablesHaveMatchingKeys(t *testing.T) {
	rates, windows := pricing.Builtin(), pricing.BuiltinContextWindows()
	for id := range rates {
		if _, ok := windows[id]; !ok {
			t.Errorf("%q has a rate but no context window — regenerate with "+
				"`go run ./dev/regen-builtin-pricing`", id)
		}
	}
	for id := range windows {
		if _, ok := rates[id]; !ok {
			t.Errorf("%q has a context window but no rate — regenerate with "+
				"`go run ./dev/regen-builtin-pricing`", id)
		}
	}
}

// TestBuiltinContextWindowsArePlausible catches a schema shift upstream
// turning max_input_tokens into something that is not a token count.
// The generator refuses to emit a non-positive window, so the interesting
// bound is the upper one: a window an order of magnitude past anything
// shipping today would push compaction triggers out past the point the
// provider hard-fails, and the symptom is a dead long session rather
// than a failed build.
func TestBuiltinContextWindowsArePlausible(t *testing.T) {
	const (
		floor   = 100_000    // no current Gemini/Anthropic chat model is smaller
		ceiling = 20_000_000 // ~10x the largest window on offer in 2026
	)
	for id, n := range pricing.BuiltinContextWindows() {
		if n < floor || n > ceiling {
			t.Errorf("BuiltinContextWindow(%q) = %d, outside the plausible "+
				"[%d, %d] range — check LiteLLM's max_input_tokens for a "+
				"units change", id, n, floor, ceiling)
		}
	}
}
