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

package modeltier_test

import (
	"testing"

	"github.com/go-steer/core-models/pricing"

	"github.com/go-steer/mast/internal/modeltier"
)

// TestBuiltinModelsKnownToCompanionTables pins the invariant that the
// model tables move together. pricing.Builtin (core-models, regenerated
// from LiteLLM) holds every Gemini and Anthropic model mast will run, so
// it is the source of truth, and the tables that answer the same
// operator-typed id fail OPEN rather than loudly:
//
//   - modeltier.Classify returns "" for an unknown model, which drops
//     the per-tier compaction threshold and leaves the small-tier parent
//     guard unable to reason about the model.
//   - a model with no context-window row falls back to a 0 sentinel,
//     which a compactor reads as "don't compact".
//
// Neither surfaces at startup. A core-models regeneration that newly
// qualifies a model, with mast's classifier left behind, is therefore a
// live defect this test turns into a build failure on the bump that
// brings it in. modeltier is mast's, so the pin lives here.
//
// SCOPE: one direction only — priced => classified and sized. The
// reverse cannot be enumerated: the classifier is a substring matcher,
// not a list.
func TestBuiltinModelsKnownToCompanionTables(t *testing.T) {
	for id := range pricing.Builtin() {
		t.Run(id, func(t *testing.T) {
			if tier := modeltier.Classify(id); tier == "" {
				t.Errorf("modeltier.Classify(%q) = \"\" (unclassified); add a case to internal/modeltier/modeltier.go", id)
			}
			if size, ok := pricing.BuiltinContextWindow(id); !ok || size == 0 {
				t.Errorf("pricing.BuiltinContextWindow(%q) = %d, ok=%v (unknown window disables threshold-based compaction)", id, size, ok)
			}
		})
	}
}
