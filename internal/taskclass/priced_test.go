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

package taskclass_test

import (
	"testing"

	"github.com/go-steer/core-models/pricing"

	"github.com/go-steer/mast/internal/taskclass"
)

// TestBuiltin_CoversTaskclassTierDefaults pins that EVERY model
// taskclass hands out — all providers, all tiers — stays priced in the
// compiled-in builtin (core-models' pricing package) across
// regenerations. An unpriced default accrues $0 cost, so the workload's
// cost ceilings never trip exactly where daemons run unattended. The
// regenerator's filter drops non-allowlisted entries silently, so
// without this pin a curation slip ships silently ("$—" across all cost
// UI) and every core-models test stays green: the defaults are mast's,
// so the pin lives here.
//
// Reached through taskclass.Providers() rather than a literal list so
// adding a provider to ModelForTier can't quietly escape coverage.
func TestBuiltin_CoversTaskclassTierDefaults(t *testing.T) {
	t.Parallel()
	builtin := pricing.Builtin()
	tiers := []string{taskclass.TierFrontier, taskclass.TierMid, taskclass.TierSmall}
	for _, provider := range taskclass.Providers() {
		for _, tier := range tiers {
			model := taskclass.ModelForTier(provider, tier)
			if model == "" {
				t.Errorf("ModelForTier(%q, %q) = \"\" — provider missing from the tier table?", provider, tier)
				continue
			}
			r, ok := builtin[model]
			if !ok {
				t.Errorf("core-models' builtin pricing is missing %q (the %s/%s tier default); it needs a row there, or a different default", model, provider, tier)
				continue
			}
			if r.InputPerMTok <= 0 || r.OutputPerMTok <= 0 {
				t.Errorf("builtin %q rates = %+v, want positive input/output", model, r)
			}
		}
	}
}
