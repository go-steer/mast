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

	"github.com/go-steer/core-models/pricing"
	"github.com/go-steer/core-models/profile"

	"github.com/go-steer/mast/pkg/budget"
)

// TestRatePer1K pins the pricing wiring: echo keeps the inflated
// offline-smoke rate, catalog-known gemini models derive from
// internal/pricing's builtin table (average of input/output per-MTok,
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
	r, ok := pricingCatalog().Lookup("gemini-3.5-flash")
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
	cr, ok := pricingCatalog().Lookup("claude-opus-4-7")
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
			if got := ratePer1K(&priceTable{cat: c}, tc.provider, tc.model); math.Abs(got-tc.want) > 1e-12 {
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
			r, ok := pricingCatalog().Lookup(p.key)
			if !ok || r.IsZero() {
				t.Fatalf("builtin catalog has no %q row; core-models must carry it (go-steer/core-models dev/regen-builtin-pricing)", p.key)
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
	if _, ok := pricingCatalog().Lookup("anthropic-vertex/claude-mythos-5"); ok {
		t.Skip("upstream now prices claude-mythos-5 on Vertex; pick another unserved pair")
	}
	bare, ok := pricingCatalog().Lookup("claude-mythos-5")
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

	if llm, err := BuildModel(ctx, "", "echo", ModelOptions{}); err != nil || llm == nil {
		t.Fatalf("BuildModel(echo) = (%v, %v), want a model", llm, err)
	}

	t.Setenv("MAST_SCRIPT", "")
	if _, err := BuildModel(ctx, "", "scripted", ModelOptions{}); err == nil || !strings.Contains(err.Error(), "MAST_SCRIPT") {
		t.Errorf("BuildModel(scripted) without MAST_SCRIPT: err = %v, want mention of MAST_SCRIPT", err)
	}

	// No creds anywhere: the claude-* error must name the recovery
	// paths (API key, Vertex project, explicit --provider).
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	if _, err := BuildModel(ctx, "", "claude-sonnet-4-6", ModelOptions{}); err == nil ||
		!strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("BuildModel(claude, no creds): err = %v, want guidance naming ANTHROPIC_API_KEY", err)
	}

	// A Gemini-family alias says nothing about Anthropic's backend, so a
	// claude-* override under a gemini root detects like the no-alias
	// path — here, with no credentials at all, that means the same
	// guidance rather than a refusal of the alias.
	if _, err := BuildModel(ctx, ProviderGemini, "claude-sonnet-4-6", ModelOptions{}); err == nil ||
		!strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("BuildModel(provider=gemini, claude-*): err = %v, want the no-alias credential guidance", err)
	}

	// An alias from neither family still refuses: nothing to detect.
	if _, err := BuildModel(ctx, "echo", "claude-sonnet-4-6", ModelOptions{}); err == nil ||
		!strings.Contains(err.Error(), "cannot serve claude-*") {
		t.Errorf("BuildModel(provider=echo, claude-*): err = %v, want a refusal naming the anthropic aliases", err)
	}

	if _, err := BuildModel(ctx, "", "gpt-42", ModelOptions{}); err == nil || !strings.Contains(err.Error(), "claude-*") {
		t.Errorf("BuildModel(gpt-42): err = %v, want the accepted-shapes enumeration", err)
	}

	// First-party anthropic constructs without network as soon as a
	// key is present (client dialing happens per-request).
	t.Setenv("ANTHROPIC_API_KEY", "test-key-not-real")
	if llm, err := BuildModel(ctx, "anthropic", "claude-sonnet-4-6", ModelOptions{}); err != nil || llm == nil {
		t.Fatalf("BuildModel(anthropic, claude-sonnet-4-6) = (%v, %v), want a model", llm, err)
	}

	// ...and so does the cross-provider override the doc comment on
	// NewModelResolver promises: a claude-* specialist under a Gemini
	// root resolves its own backend from the key it just found.
	if llm, err := BuildModel(ctx, ProviderVertex, "claude-sonnet-4-6", ModelOptions{}); err != nil || llm == nil {
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

// ---------------------------------------------------------------------
// catalogPricer: the seam between internal/pricing and pkg/budget.
//
// pkg/budget no longer names a rate type, so its own tests price against
// an explicit table and prove the derivation either side of the number.
// Everything below is the other half: that the adapter reads the real
// catalog, applies the real rates, and reports an unpriceable call as a
// miss rather than as a free one.

// probeCall is a nominal call used to ask a pricer "can you price this
// pair at all" without asserting a figure.
var probeCall = budget.Call{UncachedInputTokens: 1_000_000}

// The arithmetic, stated against the catalog's own rates rather than
// against literals so a vendor's price change is not a test failure.
func TestCatalogPricer_PricesEachBucketAtItsOwnRate(t *testing.T) {
	const name = "claude-sonnet-5"
	r, ok := pricingCatalog().Lookup(name)
	if !ok || r.IsZero() {
		t.Fatalf("builtin catalog lost %q; pick another catalog-known model", name)
	}
	if r.CachedInputPerMTok <= 0 || r.CachedInputPerMTok >= r.InputPerMTok {
		t.Fatalf("%q has no cheaper cache-read rate (%v vs input %v); this test measures nothing",
			name, r.CachedInputPerMTok, r.InputPerMTok)
	}

	c := budget.Call{UncachedInputTokens: 1_000_000, CachedInputTokens: 2_000_000, OutputTokens: 500_000}
	got, ok := builtinPricer().PriceCall("", name, c)
	if !ok {
		t.Fatalf("the builtin catalog does not price %q", name)
	}
	want := r.InputPerMTok + 2*r.CachedInputPerMTok + 0.5*r.OutputPerMTok
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("PriceCall = $%.6f, want $%.6f", got, want)
	}
	// And the cache read is what makes the difference: the same tokens
	// billed as fresh input cost strictly more. Without this the test
	// would pass against an adapter that ignored the bucket split.
	fresh, _ := builtinPricer().PriceCall("", name, budget.Call{
		UncachedInputTokens: 3_000_000, OutputTokens: 500_000,
	})
	if got >= fresh {
		t.Errorf("cache-served $%.6f is not below all-fresh $%.6f", got, fresh)
	}
}

// The cache-write bucket, same shape (#352). This is the half the
// meter's own tests cannot do: they price against a table they wrote,
// and the claim here is that the rate mast actually ships is a real,
// higher-than-input number and that the adapter routes the bucket to it.
func TestCatalogPricer_PricesCacheWritesAtTheirPremiumRate(t *testing.T) {
	const name = "claude-sonnet-5"
	r, ok := pricingCatalog().Lookup(name)
	if !ok || r.IsZero() {
		t.Fatalf("builtin catalog lost %q; pick another catalog-known model", name)
	}
	// A cache write is a premium over fresh input, not a discount — the
	// direction that makes folding it into the input bucket an undercount
	// rather than a rounding error.
	if r.CacheCreationInputPerMTok <= r.InputPerMTok {
		t.Fatalf("%q has no cache-write premium (%v vs input %v); this test measures nothing",
			name, r.CacheCreationInputPerMTok, r.InputPerMTok)
	}

	c := budget.Call{UncachedInputTokens: 1_000_000, CacheWriteTokens: 2_000_000, OutputTokens: 500_000}
	got, ok := builtinPricer().PriceCall("", name, c)
	if !ok {
		t.Fatalf("the builtin catalog does not price %q", name)
	}
	want := r.InputPerMTok + 2*r.CacheCreationInputPerMTok + 0.5*r.OutputPerMTok
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("PriceCall = $%.6f, want $%.6f", got, want)
	}
	// The pre-#352 behaviour, for contrast: the same tokens counted as
	// fresh input bill strictly less. An adapter that still passed a
	// hard-coded zero for the write bucket would land on this figure.
	folded, _ := builtinPricer().PriceCall("", name, budget.Call{
		UncachedInputTokens: 3_000_000, OutputTokens: 500_000,
	})
	if got <= folded {
		t.Errorf("cache-warming $%.6f is not above folded-as-input $%.6f; the bucket is being ignored", got, folded)
	}
}

// A catalog row that prices cache reads but never got a cache-write rate
// bills writes as fresh input. That is internal/pricing's documented fallback
// and it is the conservative direction — the old behaviour, not a free
// call — but it must be reached through the rate table rather than by
// the adapter dropping the bucket on the floor.
func TestCatalogPricer_AMissingCacheWriteRateFallsBackToInput(t *testing.T) {
	cat := mustCatalog(t, map[string]pricing.ModelRates{
		"half-priced-model": {InputPerMTok: 2, CachedInputPerMTok: 0.2, OutputPerMTok: 10},
	})
	p := catalogPricer{table: &priceTable{cat: cat}}
	got, ok := p.PriceCall("", "half-priced-model", budget.Call{CacheWriteTokens: 1_000_000})
	if !ok {
		t.Fatalf("PriceCall(row with rates) = (%v, false), want a price", got)
	}
	if want := 2.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("PriceCall = $%.6f, want the input rate $%.6f", got, want)
	}
}

// A model nobody priced is a miss, so the meter falls back to the flat
// rate and counts the call. Returning (0, true) instead would report the
// call as free, and a cost ceiling that never advances never fires.
func TestCatalogPricer_UnknownModelIsAMissNotAFreeCall(t *testing.T) {
	if usd, ok := builtinPricer().PriceCall("", "some-model-nobody-priced", probeCall); ok {
		t.Errorf("PriceCall(unknown) = ($%v, true), want a miss", usd)
	}
}

// A row that exists but carries no rates is the same kind of miss. The
// catalog can hold a placeholder — a model named in the table before its
// prices are published — and billing it at zero is the failure mode
// above with the row present.
func TestCatalogPricer_AZeroRatedRowIsAMiss(t *testing.T) {
	cat := mustCatalog(t, map[string]pricing.ModelRates{
		"placeholder-model": {},
	})
	if usd, ok := (catalogPricer{table: &priceTable{cat: cat}}).PriceCall("", "placeholder-model", probeCall); ok {
		t.Errorf("PriceCall(zero-rated row) = ($%v, true), want a miss", usd)
	}
}

// The backend half of the key selects the row, falling back to the bare
// id when the pair has none — the behaviour budget.Limits.Backend is
// documented against, measured here because every rate mast ships today
// agrees across the backends serving it.
func TestCatalogPricer_BackendQualifiedRowWinsOverTheBareOne(t *testing.T) {
	cat := mustCatalog(t, map[string]pricing.ModelRates{
		"claude-sonnet-5":                  {InputPerMTok: 2},
		"anthropic-vertex/claude-sonnet-5": {InputPerMTok: 20},
	})
	p := catalogPricer{table: &priceTable{cat: cat}}
	for _, tc := range []struct {
		backend string
		want    float64
	}{
		{"", 2.0},                  // bare row: no backend resolved
		{"anthropic", 2.0},         // no qualified row, falls back to bare
		{"anthropic-vertex", 20.0}, // its own row, ten times the price
	} {
		got, ok := p.PriceCall(tc.backend, "claude-sonnet-5", probeCall)
		if !ok {
			t.Errorf("backend %q: unpriced", tc.backend)
			continue
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("backend %q: PriceCall = $%v, want $%v", tc.backend, got, tc.want)
		}
	}
}

// TestPriceCall_LongContextAboveThreshold: the meter's pricer hands the
// whole prompt to core-models' Cost* methods, so a call above a model's
// long-context threshold bills every token at the above-threshold rates
// (gemini-2.5-pro: 200K) and one below it at the base rates.
func TestPriceCall_LongContextAboveThreshold(t *testing.T) {
	const model = "gemini-2.5-pro"
	r, ok := pricingCatalog().LookupFor("vertex", model)
	if !ok || r.LongContext == nil || r.LongContext.ThresholdTokens != 200_000 {
		t.Fatalf("%s rates = %+v, want a 200K long-context row", model, r)
	}
	p := builtinPricer()
	price := func(prompt int) float64 {
		got, ok := p.PriceCall("vertex", model, budget.Call{UncachedInputTokens: prompt, OutputTokens: 1000})
		if !ok {
			t.Fatalf("%s unpriced", model)
		}
		return got
	}
	if got, want := price(100_000), 0.1*r.InputPerMTok+0.001*r.OutputPerMTok; math.Abs(got-want) > 1e-9 {
		t.Errorf("100K prompt = $%.6f, want base rates $%.6f", got, want)
	}
	lc := r.LongContext
	if got, want := price(300_000), 0.3*lc.InputPerMTok+0.001*lc.OutputPerMTok; math.Abs(got-want) > 1e-9 {
		t.Errorf("300K prompt = $%.6f, want long-context rates $%.6f", got, want)
	}
}

// TestDeclaredRatesAreTheCatalogsLowestLayer: a profile's models[].rates
// reach the meter through the catalog, scoped to that profile's backend
// and attributed to "profile-declared".
func TestDeclaredRatesAreTheCatalogsLowestLayer(t *testing.T) {
	RegisterProfiles([]profile.Profile{
		{Name: "house", Extends: "vllm", BaseURL: "http://127.0.0.1:1/v1", Models: []profile.Model{
			{ID: "google/gemma-4-26B-A4B-it", Rates: &profile.Rates{InputPerMTok: 0.10, OutputPerMTok: 0.40}},
		}},
	})
	t.Cleanup(func() { RegisterProfiles(nil) })

	r, src, ok := pricingCatalog().LookupForWithSource("house", "google/gemma-4-26B-A4B-it")
	if !ok || r.InputPerMTok != 0.10 || r.OutputPerMTok != 0.40 || src != pricing.SourceDeclared {
		t.Errorf("declared row = %+v from %q, %v; want 0.10/0.40 from %q", r, src, ok, pricing.SourceDeclared)
	}
	if _, ok := pricingCatalog().LookupFor("vertex-maas", "google/gemma-4-26B-A4B-it"); ok {
		t.Error("a declaration priced the same id on another backend")
	}
	if got := RatePer1K("house", "google/gemma-4-26B-A4B-it"); math.Abs(got-0.00025) > 1e-12 {
		t.Errorf("RatePer1K = %v, want the declared blend 0.00025", got)
	}
}

// TestDeclaredClaudeRatesOnAProfile pins what a profile's declared
// rates for a claude-* model do, on the path production takes. A
// claude-* model under a profile alias is served by Anthropic, not by
// the profile (Backend routes it through anthropicBackend, which reads
// the environment), so the declaration is keyed on the profile's backend
// name and is reached only when that name happens to equal the Anthropic
// backend mast resolved — and even then a published Claude price beats
// it. Such declarations are close to dead config. main had the same gap
// with its compose-side declared fallback; making them meaningful would
// mean routing claude-* to a profile, which is a product decision, not
// a pricing one.
func TestDeclaredClaudeRatesOnAProfile(t *testing.T) {
	t.Setenv(EnvAnthropicAPIKey, "test-key")
	RegisterProfiles([]profile.Profile{
		{Name: "claudegw", Extends: "openai-compatible", BaseURL: "http://127.0.0.1:1/v1", Backend: ProviderAnthropic, Models: []profile.Model{
			{ID: "claude-sonnet-5", Rates: &profile.Rates{InputPerMTok: 999, OutputPerMTok: 999}},
		}},
	})
	t.Cleanup(func() { RegisterProfiles(nil) })

	const model = "claude-sonnet-5"
	backend := Backend("claudegw", model)
	if backend != ProviderAnthropic {
		t.Fatalf("Backend(claudegw, %s) = %q, want %q: claude-* under a profile is served by Anthropic", model, backend, ProviderAnthropic)
	}
	published, ok := pricing.Builtin()[model]
	if !ok {
		t.Fatalf("pricing.Builtin() has no %s row", model)
	}
	call := budget.Call{UncachedInputTokens: 1000, OutputTokens: 1000}
	got, ok := builtinPricer().PriceCall(backend, model, call)
	if want := published.CostUSDWithCacheTTLs(1000, 0, 0, 0, 1000); !ok || math.Abs(got-want) > 1e-12 {
		t.Errorf("PriceCall = $%v, %v; want the published $%v, not the declared 999", got, ok, want)
	}
	if got, want := RatePer1K("claudegw", model), (published.InputPerMTok+published.OutputPerMTok)/2/1000; math.Abs(got-want) > 1e-12 {
		t.Errorf("RatePer1K = %v, want the published blend %v", got, want)
	}
}

// TestDeclaredRatesFillAZeroRateRow: LiteLLM prices some free and
// preview models at zero. mast reads a zero row as unpriced, so the
// catalog returning it ahead of the declared layer must not hide a
// profile's declared price for the model.
func TestDeclaredRatesFillAZeroRateRow(t *testing.T) {
	tbl, err := newPriceTable(pricing.Options{
		CfgOverride: map[string]pricing.ModelRates{"lab/free-preview": {}},
		Declared:    map[string]pricing.ModelRates{"lab/free-preview": {InputPerMTok: 1, OutputPerMTok: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r, _, ok := tbl.cat.LookupForWithSource("lab", "free-preview"); !ok || !r.IsZero() {
		t.Fatalf("setup: catalog returned %+v, %v; want the zero row", r, ok)
	}
	got, ok := (catalogPricer{table: tbl}).PriceCall("lab", "free-preview", budget.Call{UncachedInputTokens: 1_000_000, OutputTokens: 1_000_000})
	if !ok || math.Abs(got-4) > 1e-9 {
		t.Errorf("PriceCall = $%v, %v; want the declared $4", got, ok)
	}
	if got := ratePer1K(tbl, "", "free-preview"); got != 0.001 {
		t.Errorf("ratePer1K with no backend = %v, want the flat 0.001: a declaration prices only its own backend", got)
	}
}

// TestDeclaredRatesFirstProfileWins: two profiles declaring the same
// model on the same backend resolve to the first in profile lookup
// order — built-ins, then declared in registration order — and a
// declared profile that overrides a built-in name holds the built-in's
// place and is counted once.
func TestDeclaredRatesFirstProfileWins(t *testing.T) {
	const model = "acme/coder-7b"
	rates := func(in float64) *profile.Rates { return &profile.Rates{InputPerMTok: in, OutputPerMTok: in} }
	RegisterProfiles([]profile.Profile{
		{Name: "lab-a", Extends: "openai-compatible", BaseURL: "http://127.0.0.1:1/v1", Backend: "lab", Models: []profile.Model{{ID: model, Rates: rates(1)}}},
		{Name: "lab-b", Extends: "openai-compatible", BaseURL: "http://127.0.0.1:1/v1", Backend: "lab", Models: []profile.Model{{ID: model, Rates: rates(2)}}},
		{Name: "house", Extends: "openai-compatible", BaseURL: "http://127.0.0.1:1/v1", Backend: "vllm", Models: []profile.Model{{ID: model, Rates: rates(3)}}},
		{Name: "vllm", BaseURL: "http://127.0.0.1:1/v1", Models: []profile.Model{{ID: model, Rates: rates(4)}}},
	})
	t.Cleanup(func() { RegisterProfiles(nil) })

	for _, tc := range []struct {
		backend string
		want    float64
		why     string
	}{
		{"lab", 1, "lab-a is registered before lab-b"},
		{"vllm", 4, "the vllm override holds the built-in's place, ahead of every declared profile"},
	} {
		r, ok := pricingTable().lookup(tc.backend, model)
		if !ok || r.InputPerMTok != tc.want {
			t.Errorf("%s/%s = %+v, %v; want %v (%s)", tc.backend, model, r, ok, tc.want, tc.why)
		}
	}
}

// TestPricerSeesProfilesRegisteredLater: a meter's pricer reads the
// table in force, so declared rates registered after it was made price
// its calls, as they do RatePer1K's.
func TestPricerSeesProfilesRegisteredLater(t *testing.T) {
	RegisterProfiles(nil)
	t.Cleanup(func() { RegisterProfiles(nil) })
	p := MeterLimits("house", "acme/late-model").Pricer
	call := budget.Call{UncachedInputTokens: 1_000_000, OutputTokens: 1_000_000}
	if usd, ok := p.PriceCall("house", "acme/late-model", call); ok {
		t.Fatalf("setup: priced at $%v before any profile declared it", usd)
	}

	RegisterProfiles([]profile.Profile{
		{Name: "house", Extends: "vllm", BaseURL: "http://127.0.0.1:1/v1", Models: []profile.Model{
			{ID: "acme/late-model", Rates: &profile.Rates{InputPerMTok: 2, OutputPerMTok: 6}},
		}},
	})
	usd, ok := p.PriceCall("house", "acme/late-model", call)
	if !ok || math.Abs(usd-8) > 1e-9 {
		t.Errorf("PriceCall after RegisterProfiles = $%v, %v; want the declared $8", usd, ok)
	}
	if got := RatePer1K("house", "acme/late-model"); math.Abs(got-0.004) > 1e-12 {
		t.Errorf("RatePer1K = %v, want the declared blend 0.004", got)
	}
}
