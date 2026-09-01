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
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/pricing"
)

// TestRatePer1K pins the pricing wiring: echo keeps the inflated
// offline-smoke rate, catalog-known gemini models derive from
// pkg/pricing's builtin table (average of input/output per-MTok,
// scaled to per-1K), and catalog misses keep the pre-catalog flat
// spike rate instead of dropping to zero.
func TestRatePer1K(t *testing.T) {
	// Backend resolution reads the environment, so a claude-* rate is
	// only deterministic once the credentials are pinned. Pin them to
	// "none": Backend then declines to name a backend and every claude
	// lookup below is the bare-id path this test has always measured.
	// The (backend, model) paths get their own test.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "")

	if got := RatePer1K("", "echo"); got != 0.05 {
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
	if got := RatePer1K("", "gemini-3.5-flash"); math.Abs(got-want) > 1e-12 {
		t.Errorf("RatePer1K(gemini-3.5-flash) = %v, want %v (builtin catalog blend)", got, want)
	}
	if want <= 0 {
		t.Errorf("derived gemini rate %v is not positive; budget metering would be free", want)
	}

	// Longest-prefix behavior rides along from pricing.Lookup: a
	// dated variant of a catalog model prices like its base ID.
	if got := RatePer1K("", "gemini-3.5-flash-20260520"); math.Abs(got-want) > 1e-12 {
		t.Errorf("RatePer1K(dated gemini-3.5-flash) = %v, want %v (prefix match)", got, want)
	}

	// Catalog miss: the pre-catalog flat spike rate, never zero.
	if got := RatePer1K("", "gemini-9.9-imaginary"); got != 0.0006 {
		t.Errorf("RatePer1K(unknown gemini) = %v, want 0.0006 fallback", got)
	}

	// Claude models price from the same catalog blend (P1.3b).
	cr, ok := builtinCatalog().Lookup("claude-opus-4-7")
	if !ok || cr.IsZero() {
		t.Fatal("builtin catalog lost claude-opus-4-7; pick another catalog-known model for this test")
	}
	cwant := (cr.InputPerMTok + cr.OutputPerMTok) / 2 / 1000
	if got := RatePer1K("", "claude-opus-4-7"); math.Abs(got-cwant) > 1e-12 {
		t.Errorf("RatePer1K(claude-opus-4-7) = %v, want %v (builtin catalog blend)", got, cwant)
	}
	if got := RatePer1K("", "claude-99-imaginary"); got != 0.003 {
		t.Errorf("RatePer1K(unknown claude) = %v, want 0.003 fallback", got)
	}

	// The scripted replay shares echo's inflated offline rate.
	if got := RatePer1K("", "scripted"); got != 0.05 {
		t.Errorf("RatePer1K(scripted) = %v, want 0.05 (offline test double)", got)
	}

	if got := RatePer1K("", "something-else"); got != 0.001 {
		t.Errorf("RatePer1K(non-gemini) = %v, want 0.001", got)
	}
}

// TestBackendResolvesTheBillingBackend pins the mapping a price is keyed
// on. The alias is not the answer on its own: three of these cases carry
// no alias at all and still resolve to a specific backend, which is the
// whole reason pricing asks Backend rather than reading the flag.
func TestBackendResolvesTheBillingBackend(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		env      map[string]string
		want     string
	}{
		{"explicit anthropic", "anthropic", "claude-opus-5", nil, "anthropic"},
		{"explicit anthropic-vertex", "anthropic-vertex", "claude-opus-5", nil, "anthropic-vertex"},
		{"no alias, api key present", "", "claude-opus-5",
			map[string]string{"ANTHROPIC_API_KEY": "k"}, "anthropic"},
		{"no alias, only a vertex project", "", "claude-opus-5",
			map[string]string{"GOOGLE_CLOUD_PROJECT": "p"}, "anthropic-vertex"},
		{"api key wins over a vertex project", "", "claude-opus-5",
			map[string]string{"ANTHROPIC_API_KEY": "k", "GOOGLE_CLOUD_PROJECT": "p"}, "anthropic"},
		// A gemini alias says nothing about Anthropic's backend; the
		// environment still decides, exactly as BuildModel does.
		{"gemini alias over a claude model", ProviderGemini, "claude-opus-5",
			map[string]string{"GOOGLE_CLOUD_PROJECT": "p"}, "anthropic-vertex"},
		// Unresolvable: BuildModel is about to fail on the same
		// condition, so "" (price by bare id) is not a wrong price, it
		// is a price nobody will get to spend.
		{"claude with no credentials at all", "", "claude-opus-5", nil, ""},
		{"explicit vertex", ProviderVertex, "gemini-3.7-flash", nil, "vertex"},
		{"explicit gemini", ProviderGemini, "gemini-3.7-flash", nil, "gemini"},
		{"no alias, developer api", "", "gemini-3.7-flash", nil, "gemini"},
		{"no alias, GOOGLE_GENAI_USE_VERTEXAI", "", "gemini-3.7-flash",
			map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true"}, "vertex"},
		{"offline fake", "anthropic", "echo", nil, ""},
		{"model in neither family", "", "gpt-42", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_VERTEX_PROJECT_ID",
				"GOOGLE_CLOUD_PROJECT", "GOOGLE_GENAI_USE_VERTEXAI"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := Backend(tc.provider, tc.model); got != tc.want {
				t.Errorf("Backend(%q, %q) = %q, want %q", tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

// The discriminating test: two backends, two prices, one model id.
//
// Nothing built from the real builtin table can catch a regression here.
// Every model mast ships costs the same first-party and on Vertex, so a
// RatePer1K that threw the backend away would return the identical
// number for every case below. This one prices the pairs apart in a
// synthetic catalog, which is the only way to see which key was read —
// and the divergence it invents is precisely the event the pair-keyed
// rate exists to survive.
func TestRatePer1K_FollowsTheResolvedBackend(t *testing.T) {
	c := mustCatalog(t, map[string]pricing.ModelRates{
		"claude-x":                  {InputPerMTok: 10, OutputPerMTok: 10},
		"anthropic/claude-x":        {InputPerMTok: 20, OutputPerMTok: 20},
		"anthropic-vertex/claude-x": {InputPerMTok: 30, OutputPerMTok: 30},
		"gemini-x":                  {InputPerMTok: 40, OutputPerMTok: 40},
		"gemini/gemini-x":           {InputPerMTok: 50, OutputPerMTok: 50},
		"vertex/gemini-x":           {InputPerMTok: 60, OutputPerMTok: 60},
	})

	tests := []struct {
		name     string
		provider string
		model    string
		env      map[string]string
		want     float64 // per-MTok blend / 1000
	}{
		{"claude first-party by key", "", "claude-x",
			map[string]string{"ANTHROPIC_API_KEY": "k"}, 20.0 / 1000},
		{"claude on vertex by env", "", "claude-x",
			map[string]string{"GOOGLE_CLOUD_PROJECT": "p"}, 30.0 / 1000},
		{"claude with no credentials falls back to the bare row", "", "claude-x",
			nil, 10.0 / 1000},
		{"gemini developer api", "", "gemini-x", nil, 50.0 / 1000},
		{"gemini on vertex by env", "", "gemini-x",
			map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "1"}, 60.0 / 1000},
		{"gemini on vertex by alias", ProviderVertex, "gemini-x", nil, 60.0 / 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_VERTEX_PROJECT_ID",
				"GOOGLE_CLOUD_PROJECT", "GOOGLE_GENAI_USE_VERTEXAI"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := ratePer1K(c, tc.provider, tc.model); math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("ratePer1K(%q, %q) = %v, want %v", tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

func mustCatalog(t *testing.T, rates map[string]pricing.ModelRates) *pricing.Catalog {
	t.Helper()
	c, err := pricing.NewCatalog(pricing.Options{CfgOverride: rates})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return c
}

// TestRatePer1KPricesThePair is the same claim against the shipped
// table: the rate comes from the backend-qualified row when one exists.
//
// It cannot assert that the two backends differ — they do not, today,
// for anything mast ships. What it pins is that the qualified rows are
// present and reachable for the pairs mast actually runs, so the
// divergence the test above simulates would land on a real key rather
// than on nothing.
func TestRatePer1KPricesThePair(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "")

	blend := func(r pricing.Rates) float64 { return (r.InputPerMTok + r.OutputPerMTok) / 2 / 1000 }

	pairs := []struct{ provider, model, key string }{
		{"anthropic", "claude-opus-5", "anthropic/claude-opus-5"},
		{"anthropic-vertex", "claude-opus-5", "anthropic-vertex/claude-opus-5"},
		{ProviderGemini, "gemini-3.7-flash", "gemini/gemini-3.7-flash"},
		{ProviderVertex, "gemini-3.7-flash", "vertex/gemini-3.7-flash"},
	}
	for _, p := range pairs {
		t.Run(p.key, func(t *testing.T) {
			r, ok := builtinCatalog().Lookup(p.key)
			if !ok || r.IsZero() {
				t.Fatalf("builtin catalog has no %q row; regenerate with `go run ./dev/regen-builtin-pricing`", p.key)
			}
			if got := RatePer1K(p.provider, p.model); math.Abs(got-blend(r)) > 1e-12 {
				t.Errorf("RatePer1K(%q, %q) = %v, want %v (the %q row)",
					p.provider, p.model, got, blend(r), p.key)
			}
		})
	}

	// A pair no backend table prices falls back to the bare id rather
	// than to a zero or to the flat miss rate. Vertex does not serve
	// claude-mythos-5, so there is no anthropic-vertex row for it — and
	// a workload that asks for one must still get a real number.
	if _, ok := builtinCatalog().Lookup("anthropic-vertex/claude-mythos-5"); ok {
		t.Skip("upstream now prices claude-mythos-5 on Vertex; pick another unserved pair")
	}
	bare, ok := builtinCatalog().Lookup("claude-mythos-5")
	if !ok || bare.IsZero() {
		t.Fatal("builtin catalog lost claude-mythos-5")
	}
	if got := RatePer1K("anthropic-vertex", "claude-mythos-5"); math.Abs(got-blend(bare)) > 1e-12 {
		t.Errorf("RatePer1K(anthropic-vertex, claude-mythos-5) = %v, want the bare-id %v; "+
			"an unserved pair must fall back to a real rate, not to a miss", got, blend(bare))
	}
}

// TestBuildModel_ErrorsAndMocks pins BuildModel's credential-free
// paths: the mocks construct, scripted demands MAST_SCRIPT, claude-*
// without credentials names every way to supply them, and unknown
// names enumerate the accepted shapes.
func TestBuildModel_ErrorsAndMocks(t *testing.T) {
	ctx := t.Context()

	if llm, err := BuildModel(ctx, "", "echo"); err != nil || llm == nil {
		t.Fatalf("BuildModel(echo) = (%v, %v), want a model", llm, err)
	}

	t.Setenv("MAST_SCRIPT", "")
	if _, err := BuildModel(ctx, "", "scripted"); err == nil || !strings.Contains(err.Error(), "MAST_SCRIPT") {
		t.Errorf("BuildModel(scripted) without MAST_SCRIPT: err = %v, want mention of MAST_SCRIPT", err)
	}

	// No creds anywhere: the claude-* error must name the recovery
	// paths (API key, Vertex project, explicit --provider).
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	if _, err := BuildModel(ctx, "", "claude-sonnet-4-6"); err == nil ||
		!strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("BuildModel(claude, no creds): err = %v, want guidance naming ANTHROPIC_API_KEY", err)
	}

	// A Gemini-family alias says nothing about Anthropic's backend, so a
	// claude-* override under a gemini root detects like the no-alias
	// path — here, with no credentials at all, that means the same
	// guidance rather than a refusal of the alias.
	if _, err := BuildModel(ctx, ProviderGemini, "claude-sonnet-4-6"); err == nil ||
		!strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("BuildModel(provider=gemini, claude-*): err = %v, want the no-alias credential guidance", err)
	}

	// An alias from neither family still refuses: nothing to detect.
	if _, err := BuildModel(ctx, "echo", "claude-sonnet-4-6"); err == nil ||
		!strings.Contains(err.Error(), "cannot serve claude-*") {
		t.Errorf("BuildModel(provider=echo, claude-*): err = %v, want a refusal naming the anthropic aliases", err)
	}

	if _, err := BuildModel(ctx, "", "gpt-42"); err == nil || !strings.Contains(err.Error(), "claude-*") {
		t.Errorf("BuildModel(gpt-42): err = %v, want the accepted-shapes enumeration", err)
	}

	// First-party anthropic constructs without network as soon as a
	// key is present (client dialing happens per-request).
	t.Setenv("ANTHROPIC_API_KEY", "test-key-not-real")
	if llm, err := BuildModel(ctx, "anthropic", "claude-sonnet-4-6"); err != nil || llm == nil {
		t.Fatalf("BuildModel(anthropic, claude-sonnet-4-6) = (%v, %v), want a model", llm, err)
	}

	// ...and so does the cross-provider override the doc comment on
	// NewModelResolver promises: a claude-* specialist under a Gemini
	// root resolves its own backend from the key it just found.
	if llm, err := BuildModel(ctx, ProviderVertex, "claude-sonnet-4-6"); err != nil || llm == nil {
		t.Fatalf("BuildModel(provider=vertex, claude-sonnet-4-6) = (%v, %v), want a model", llm, err)
	}
}

// TestBuiltinCatalogConstructs guards the OnceValue panic path: empty
// Options must never fail.
func TestBuiltinCatalogConstructs(t *testing.T) {
	if _, err := pricing.NewCatalog(pricing.Options{}); err != nil {
		t.Fatalf("NewCatalog(empty): %v", err)
	}
}
