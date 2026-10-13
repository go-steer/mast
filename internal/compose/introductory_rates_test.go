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

package compose

import (
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-models/pricing"
)

// TestIntroductoryRatesHaveNotOutlivedTheirExpiry keeps mast's own build
// red when a promotional rate mast bills from has lapsed, whatever
// core-models version is pinned. core-models runs the same check in its
// own CI, but that can't stop mast shipping a pin whose table is past an
// expiry.
//
// The list of promotional rates, their dates and the judgment
// (IntroductoryRate.Check: fine before expiry, fail after it while the
// row is unchanged, pinned at the former rate when Resolved) live in
// core-models (pricing.IntroductoryRates). What mast adds is which
// backends it serves each model on, checked through mast's own lookup,
// since that is the row the meter actually bills from.
func TestIntroductoryRatesHaveNotOutlivedTheirExpiry(t *testing.T) {
	now := time.Now().UTC()
	for _, ir := range pricing.IntroductoryRates() {
		backends := introductoryBackends(ir.Model)
		if len(backends) == 0 {
			t.Errorf("%s: no mast backend serves this model family; teach introductoryBackends about it "+
				"so a new entry in core-models can't be skipped silently", ir.Model)
			continue
		}
		for _, backend := range backends {
			t.Run(backend+"/"+ir.Model, func(t *testing.T) {
				r, ok := pricingTable().lookup(backend, ir.Model)
				if !ok {
					t.Skipf("%s has no price on %s; the entry is noise for this backend", ir.Model, backend)
				}
				v := ir.Check(r, now)
				switch {
				case v.Fail:
					t.Errorf("%s on %s: %s\nFixes to the table land in go-steer/core-models; bump mast's pin once it has the new rate.",
						ir.Model, backend, v.Message)
				case v.Message != "":
					t.Logf("%s on %s: %s", ir.Model, backend, v.Message)
				}
			})
		}
	}
}

// introductoryBackends is the backends mast serves a model on, by family.
func introductoryBackends(model string) []string {
	switch {
	case strings.HasPrefix(model, "claude-"):
		return []string{ProviderAnthropic, ProviderAnthropicVertex}
	case strings.HasPrefix(model, "gemini-"):
		return []string{ProviderGemini, ProviderVertex}
	}
	return nil
}
