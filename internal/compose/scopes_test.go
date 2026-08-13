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
	scopes := MeterScopes(specs, "gemini-3.5-flash")
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
	}}, "gemini-3.5-flash")
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
	}, root)
	got, ok := scopes["analyst"]
	if !ok {
		t.Fatal("a model override alone should mint a scope: it changes the price")
	}
	want := RatePer1K("claude-haiku-4-5")
	if math.Abs(got.RatePer1K-want) > 1e-12 {
		t.Errorf("analyst rate = %v, want haiku's %v", got.RatePer1K, want)
	}
	if parent := RatePer1K(root); math.Abs(got.RatePer1K-parent) < 1e-12 {
		t.Fatalf("fixture is not discriminating: haiku and %s price identically at %v", root, parent)
	}
}

// A specialist with no override inherits the session rate, which the
// meter spells as a zero rate on the scope. Writing the parent's rate
// in would work today and break the moment the session rate is
// overridden (mast.Config.Budget does exactly that).
func TestMeterScopes_NoOverrideLeavesTheRateInherited(t *testing.T) {
	scopes := MeterScopes([]specialists.Spec{
		{Name: "capped", Budget: specialists.Budget{MaxCostUSD: 1}},
	}, "claude-opus-4-7")
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
	}}
	for _, root := range []string{"echo", "mast-echo", "scripted", "toolactor", "mast-toolactor"} {
		t.Run(root, func(t *testing.T) {
			scopes := MeterScopes(specs, root)
			if got := scopes["analyst"].RatePer1K; got != 0 {
				t.Errorf("rate = %v under fake root %q, want 0 (inherit the fake's rate)", got, root)
			}
			// The ceiling still applies: only the price collapses.
			if got := scopes["analyst"].MaxCostUSD; got != 2.50 {
				t.Errorf("MaxCostUSD = %v, want the declared 2.50 — a fake root collapses pricing, not ceilings", got)
			}
		})
	}
}

func TestMeterScopes_EmptyRosterIsNil(t *testing.T) {
	if got := MeterScopes(nil, "echo"); got != nil {
		t.Errorf("MeterScopes(nil) = %v, want nil", got)
	}
	if got := MeterScopes([]specialists.Spec{{Name: "plain"}}, "echo"); got != nil {
		t.Errorf("a roster with nothing to declare should mint no map, got %v", got)
	}
}
