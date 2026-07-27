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
	"math"
	"testing"

	"github.com/go-steer/mast/pkg/pricing"
)

// TestRatePer1K pins the pricing wiring: echo keeps the inflated
// offline-smoke rate, catalog-known gemini models derive from
// pkg/pricing's builtin table (average of input/output per-MTok,
// scaled to per-1K), and catalog misses keep the pre-catalog flat
// spike rate instead of dropping to zero.
func TestRatePer1K(t *testing.T) {
	if got := RatePer1K("echo"); got != 0.05 {
		t.Errorf("RatePer1K(echo) = %v, want 0.05 (offline smoke tests trip caps with it)", got)
	}

	// gemini-3.5-flash is in the builtin catalog; the flat rate is
	// the blended average, computed from the catalog rather than
	// hard-coded so a builtin regen doesn't break this test.
	r, ok := builtinCatalog().Lookup("gemini-3.5-flash")
	if !ok || r.IsZero() {
		t.Fatal("builtin catalog lost gemini-3.5-flash; pick another catalog-known model for this test")
	}
	want := (r.InputPerMTok + r.OutputPerMTok) / 2 / 1000
	if got := RatePer1K("gemini-3.5-flash"); math.Abs(got-want) > 1e-12 {
		t.Errorf("RatePer1K(gemini-3.5-flash) = %v, want %v (builtin catalog blend)", got, want)
	}
	if want <= 0 {
		t.Errorf("derived gemini rate %v is not positive; budget metering would be free", want)
	}

	// Longest-prefix behavior rides along from pricing.Lookup: a
	// dated variant of a catalog model prices like its base ID.
	if got := RatePer1K("gemini-3.5-flash-20260520"); math.Abs(got-want) > 1e-12 {
		t.Errorf("RatePer1K(dated gemini-3.5-flash) = %v, want %v (prefix match)", got, want)
	}

	// Catalog miss: the pre-catalog flat spike rate, never zero.
	if got := RatePer1K("gemini-9.9-imaginary"); got != 0.0006 {
		t.Errorf("RatePer1K(unknown gemini) = %v, want 0.0006 fallback", got)
	}

	if got := RatePer1K("something-else"); got != 0.001 {
		t.Errorf("RatePer1K(non-gemini) = %v, want 0.001", got)
	}
}

// TestBuiltinCatalogConstructs guards the OnceValue panic path: empty
// Options must never fail.
func TestBuiltinCatalogConstructs(t *testing.T) {
	if _, err := pricing.NewCatalog(pricing.Options{}); err != nil {
		t.Fatalf("NewCatalog(empty): %v", err)
	}
}
