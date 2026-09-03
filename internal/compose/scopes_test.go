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

	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/specialists"
)

// A specialist that declares nothing the meter can act on gets no
// scope. This is what keeps an ordinary roster metering exactly as it
// did before scopes existed — and it is the difference between "no
// ceiling" and "a ceiling of zero".
func TestMeterScopes_OnlyForSpecialistsThatDeclareSomething(t *testing.T) {
	specs := []specialists.Spec{
		{Name: "plain"},
		{Name: "wallclock-only", Budget: specialists.Budget{MaxWallclockSeconds: 30}},
		{Name: "capped", Budget: specialists.Budget{MaxCostUSD: 2.50}},
	}
	scopes := MeterScopes(specs, "", "gemini-3.5-flash")
	if len(scopes) != 1 {
		t.Fatalf("scopes = %v, want just the capped specialist", scopes)
	}
	got, ok := scopes["capped"]
	if !ok {
		t.Fatal(`no scope for "capped"`)
	}
	if got != (budget.Limits{MaxCostUSD: 2.50}) {
		t.Errorf("scope = %+v, want only the declared cost cap", got)
	}
	// max_wallclock_seconds is a node-level knob (pkg/graph maps it onto
	// workflow.NodeConfig.Timeout); a usage meter cannot see wallclock,
	// so declaring only that must not mint a scope with no ceilings in
	// it.
	if _, ok := scopes["wallclock-only"]; ok {
		t.Error("max_wallclock_seconds minted a budget scope; it is a node timeout, not a usage ceiling")
	}
}

func TestMeterScopes_CarriesTurnsAndCost(t *testing.T) {
	scopes := MeterScopes([]specialists.Spec{{
		Name:   "OOMKilled",
		Budget: specialists.Budget{MaxTurns: 4, MaxCostUSD: 2.50, MaxWallclockSeconds: 60},
	}}, "", "gemini-3.5-flash")
	want := budget.Limits{MaxTurns: 4, MaxCostUSD: 2.50}
	if got := scopes["OOMKilled"]; got != want {
		t.Errorf("scope = %+v, want %+v", got, want)
	}
}

// A `model:` override prices that specialist's tokens at its own tier
// rather than the parent's — the attribution that makes a tiered roster
// measurable.
func TestMeterScopes_OverridePricesAtItsOwnTier(t *testing.T) {
	const root = "claude-opus-4-7"
	scopes := MeterScopes([]specialists.Spec{
		{Name: "analyst", Model: "claude-haiku-4-5"},
	}, "", root)
	got, ok := scopes["analyst"]
	if !ok {
		t.Fatal("a model override alone should mint a scope: it changes the price")
	}
	want := RatePer1K("", "claude-haiku-4-5")
	if math.Abs(got.RatePer1K-want) > 1e-12 {
		t.Errorf("analyst rate = %v, want haiku's %v", got.RatePer1K, want)
	}
	if parent := RatePer1K("", root); math.Abs(got.RatePer1K-parent) < 1e-12 {
		t.Fatalf("fixture is not discriminating: haiku and %s price identically at %v", root, parent)
	}
}

// The same attribution for the portable spelling. This is the half
// that is easy to ship broken: `tier:` resolves at build time, so the
// specialist really does run on the cheap model, and a meter that
// priced off `model:` alone would keep billing it at the root's rate
// while the audit log shows the cheap one. The bundle would be right,
// the run would be right, and the number would be wrong.
func TestMeterScopes_TierPricesAtItsResolvedModel(t *testing.T) {
	const root = "claude-opus-4-7"
	scopes := MeterScopes([]specialists.Spec{
		{Name: "diagnoser", Tier: "small"},
	}, "", root)
	got, ok := scopes["diagnoser"]
	if !ok {
		t.Fatal("a tier alone should mint a scope: it changes the price")
	}
	want := RatePer1K("", "claude-haiku-4-5") // the small tier for a claude root
	if math.Abs(got.RatePer1K-want) > 1e-12 {
		t.Errorf("diagnoser rate = %v, want the small tier's %v", got.RatePer1K, want)
	}
	if parent := RatePer1K("", root); math.Abs(got.RatePer1K-parent) < 1e-12 {
		t.Fatalf("fixture is not discriminating: the small tier and %s price identically at %v", root, parent)
	}
}

// A tier that cannot be resolved for the running provider does not
// invent a price. BuildRoot has already refused such a roster by the
// time anything is metered; if that ever stops being true, an inherited
// rate is the honest answer, not a guess at what the tier meant.
func TestMeterScopes_UnresolvableTierInheritsTheRate(t *testing.T) {
	scopes := MeterScopes([]specialists.Spec{
		{Name: "diagnoser", Tier: "small", Budget: specialists.Budget{MaxCostUSD: 1}},
	}, "", "some-unrecognized-model")
	if got := scopes["diagnoser"].RatePer1K; got != 0 {
		t.Errorf("rate = %v, want 0 (inherit the session's)", got)
	}
}

// A specialist with no override inherits the session rate, which the
// meter spells as a zero rate on the scope. Writing the parent's rate
// in would work today and break the moment the session rate is
// overridden (mast.Config.Budget does exactly that).
func TestMeterScopes_NoOverrideLeavesTheRateInherited(t *testing.T) {
	scopes := MeterScopes([]specialists.Spec{
		{Name: "capped", Budget: specialists.Budget{MaxCostUSD: 1}},
	}, "", "claude-opus-4-7")
	if got := scopes["capped"].RatePer1K; got != 0 {
		t.Errorf("rate = %v, want 0 (inherit the session's)", got)
	}
}

// Pricing collapses under an offline fake on the same condition
// NewModelResolver collapses the models: every override resolved back
// to the fake, so every token was produced by the fake, and pricing a
// specialist at a tier that never ran would report a cost that did not
// happen.
func TestMeterScopes_OfflineFakeRootCollapsesPricing(t *testing.T) {
	specs := []specialists.Spec{{
		Name:   "analyst",
		Model:  "claude-haiku-4-5",
		Budget: specialists.Budget{MaxCostUSD: 2.50},
	}, {
		Name:   "diagnoser",
		Tier:   "small",
		Budget: specialists.Budget{MaxCostUSD: 2.50},
	}}
	for _, root := range []string{"echo", "mast-echo", "scripted", "toolactor", "mast-toolactor"} {
		t.Run(root, func(t *testing.T) {
			scopes := MeterScopes(specs, "", root)
			for _, name := range []string{"analyst", "diagnoser"} {
				if got := scopes[name].RatePer1K; got != 0 {
					t.Errorf("%s rate = %v under fake root %q, want 0 (inherit the fake's rate)", name, got, root)
				}
				// The ceiling still applies: only the price collapses.
				if got := scopes[name].MaxCostUSD; got != 2.50 {
					t.Errorf("%s MaxCostUSD = %v, want the declared 2.50 — a fake root collapses pricing, not ceilings", name, got)
				}
			}
		})
	}
}

// A scope that names a model must carry the catalog too. The name alone
// is inert — budget.Limits.Model is documented as "ignored unless
// Catalog is set" — so a scope with one and not the other prices at the
// flat rate while looking like it prices exactly.
func TestMeterScopes_OverrideCarriesTheCatalogWithTheName(t *testing.T) {
	const override = "claude-haiku-4-5"
	scopes := MeterScopes([]specialists.Spec{
		{Name: "analyst", Model: override},
	}, "", "claude-opus-4-7")
	got := scopes["analyst"]
	if got.Model != override {
		t.Errorf("scope Model = %q, want the resolved override", got.Model)
	}
	if got.Catalog == nil {
		t.Fatal("scope has a model name and no catalog: the name prices nothing on its own")
	}
	// The backend travels with the name, because the catalog is keyed by
	// the pair. Asserted against Backend rather than a literal: which
	// backend serves Claude depends on the environment's credentials, and
	// the invariant under test is that the scope agrees with the resolver
	// the client is built from, not which one this machine happens to
	// have.
	if want := Backend("", override); got.Backend != want {
		t.Errorf("scope Backend = %q, want %q", got.Backend, want)
	}
	if r, ok := got.Catalog.LookupFor(got.Backend, got.Model); !ok || r.IsZero() {
		t.Errorf("the scope's own (backend, model) pair (%q, %q) prices nothing", got.Backend, got.Model)
	}
}

// The offline collapse covers the catalog for the same reason it covers
// the rate — and for one more: a fake's model ID is in no catalog, so
// leaving the catalog installed would make every fake call a miss and
// climb budget.Meter.Unpriced, telling an operator their cost figure was
// degraded when it was never meant to be a cost figure at all.
func TestMeterScopes_OfflineFakeInstallsNoCatalog(t *testing.T) {
	scopes := MeterScopes([]specialists.Spec{
		{Name: "analyst", Model: "claude-haiku-4-5", Budget: specialists.Budget{MaxCostUSD: 2.50}},
	}, "", "echo")
	got := scopes["analyst"]
	if got.Catalog != nil || got.Model != "" || got.Backend != "" {
		t.Errorf("scope = %+v, want no pricing under a fake root", got)
	}
}

// MeterLimits is the session half, and the reason it exists is that
// there were two hand-written copies of it. The catalog and the pair it
// is keyed by have to come out together, or the catalog is installed
// with nothing to look up.
func TestMeterLimits_CarriesCatalogNameAndFallbackRate(t *testing.T) {
	const name = "gemini-3.7-flash"
	got := MeterLimits(ProviderGemini, name)
	if got.Catalog == nil {
		t.Fatal("no catalog: every call would meter at the flat rate")
	}
	if got.Model != name {
		t.Errorf("Model = %q, want %q — the catalog has no key without it", got.Model, name)
	}
	if want := Backend(ProviderGemini, name); got.Backend != want {
		t.Errorf("Backend = %q, want %q", got.Backend, want)
	}
	if want := RatePer1K(ProviderGemini, name); math.Abs(got.RatePer1K-want) > 1e-12 {
		t.Errorf("RatePer1K = %v, want %v (the fallback for a call the catalog misses)", got.RatePer1K, want)
	}
	// The pair has to actually resolve, or the fields are decoration.
	if r, ok := got.Catalog.LookupFor(got.Backend, name); !ok || r.IsZero() {
		t.Errorf("the builtin catalog does not price %q on %q", name, got.Backend)
	}
}

func TestMeterLimits_OfflineFakeKeepsOnlyTheFakeRate(t *testing.T) {
	for _, name := range []string{"echo", "scripted", "toolactor"} {
		got := MeterLimits("", name)
		if got.Catalog != nil || got.Model != "" || got.Backend != "" {
			t.Errorf("%s: limits = %+v, want no catalog pricing", name, got)
		}
		if got.RatePer1K != 0.05 {
			t.Errorf("%s: RatePer1K = %v, want the inflated fake rate a smoke test trips caps with", name, got.RatePer1K)
		}
	}
}

func TestMeterScopes_EmptyRosterIsNil(t *testing.T) {
	if got := MeterScopes(nil, "", "echo"); got != nil {
		t.Errorf("MeterScopes(nil) = %v, want nil", got)
	}
	if got := MeterScopes([]specialists.Spec{{Name: "plain"}}, "", "echo"); got != nil {
		t.Errorf("a roster with nothing to declare should mint no map, got %v", got)
	}
}
