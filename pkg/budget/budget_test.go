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

package budget

import (
	"errors"
	"math"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/pricing"
)

// usageEvent fakes one model call's worth of streamed usage — the
// shape the meter sees from the runner event stream.
func usageEvent(totalTokens int32) *session.Event {
	return &session.Event{
		LLMResponse: model.LLMResponse{
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				TotalTokenCount: totalTokens,
			},
		},
	}
}

// authoredEvent is a usage event attributed to one agent — the shape
// the meter buckets scopes on (session.Event.Author).
func authoredEvent(author string, totalTokens int32) *session.Event {
	ev := usageEvent(totalTokens)
	ev.Author = author
	return ev
}

func TestMaxTurnsTrips(t *testing.T) {
	m := NewMeter(Limits{MaxTurns: 2})
	if err := m.Observe(usageEvent(10)); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if err := m.Observe(usageEvent(10)); err != nil {
		t.Fatalf("turn 2 (at cap, not over): %v", err)
	}
	err := m.Observe(usageEvent(10))
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("turn 3: want ErrExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "turns") {
		t.Errorf("error should name the turn cap: %v", err)
	}
	if _, _, calls := m.Snapshot(); calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// Events without UsageMetadata (function responses, control events)
// are free: they are neither billed nor counted as turns.
func TestNonModelEventsAreNotTurns(t *testing.T) {
	m := NewMeter(Limits{MaxTurns: 1})
	for i := 0; i < 5; i++ {
		if err := m.Observe(&session.Event{}); err != nil {
			t.Fatalf("free event %d: %v", i, err)
		}
	}
	if err := m.Observe(nil); err != nil {
		t.Fatalf("nil event: %v", err)
	}
	if err := m.Observe(usageEvent(10)); err != nil {
		t.Fatalf("first real turn should be under a cap of 1: %v", err)
	}
}

func TestZeroMaxTurnsMeansUnlimited(t *testing.T) {
	m := NewMeter(Limits{})
	for i := 0; i < 100; i++ {
		if err := m.Observe(usageEvent(10)); err != nil {
			t.Fatalf("turn %d with no limits: %v", i, err)
		}
	}
}

func TestCostCapTrips(t *testing.T) {
	// 1000 tokens/call at $0.05/1K = $0.05/call; a $0.12 cap survives
	// two calls and trips on the third.
	m := NewMeter(Limits{MaxCostUSD: 0.12, RatePer1K: 0.05})
	if err := m.Observe(usageEvent(1000)); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if err := m.Observe(usageEvent(1000)); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if err := m.Observe(usageEvent(1000)); !errors.Is(err, ErrExceeded) {
		t.Fatalf("call 3: want ErrExceeded, got %v", err)
	}
}

func TestTokenCapTrips(t *testing.T) {
	m := NewMeter(Limits{MaxTokens: 150})
	if err := m.Observe(usageEvent(100)); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if err := m.Observe(usageEvent(100)); !errors.Is(err, ErrExceeded) {
		t.Fatalf("call 2: want ErrExceeded, got %v", err)
	}
}

// ---------------------------------------------------------------------
// Scopes: per-specialist ceilings composed under the session's.

// A specialist that declares a tighter cost cap than its workload stops
// on its own ceiling, and the error says whose it was — the W1.2
// invariant, in the unit the enforcement lives in.
func TestScopeCostCapStopsBeforeTheSessionCap(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxCostUSD: 100, RatePer1K: 1.0},
		Scopes: map[string]Limits{"OOMKilled": {MaxCostUSD: 2.50}},
	})
	// $1.00 per call: two calls sit under the specialist's $2.50, the
	// third crosses it while the workload's $100 is barely touched.
	for i := 1; i <= 2; i++ {
		if err := m.Observe(authoredEvent("OOMKilled", 1000)); err != nil {
			t.Fatalf("specialist call %d: %v", i, err)
		}
	}
	err := m.Observe(authoredEvent("OOMKilled", 1000))
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("specialist call 3: want ErrExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), `specialist "OOMKilled"`) {
		t.Errorf("the error must name the specialist whose ceiling stopped the run: %v", err)
	}
	if _, cost, _ := m.Snapshot(); cost > 100 {
		t.Fatalf("session cost %v crossed its own cap; the specialist's cap is not what stopped this", cost)
	}
}

func TestScopeTurnCapStopsThatSpecialistOnly(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxTurns: 50},
		Scopes: map[string]Limits{"classifier": {MaxTurns: 1}},
	})
	// The coordinator is unscoped: it may spend freely under the
	// session's ceiling.
	for i := 0; i < 10; i++ {
		if err := m.Observe(authoredEvent("triage_coordinator", 10)); err != nil {
			t.Fatalf("unscoped author call %d: %v", i, err)
		}
	}
	if err := m.Observe(authoredEvent("classifier", 10)); err != nil {
		t.Fatalf("classifier call 1 (at cap, not over): %v", err)
	}
	err := m.Observe(authoredEvent("classifier", 10))
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("classifier call 2: want ErrExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "turns") || !strings.Contains(err.Error(), "classifier") {
		t.Errorf("error should name the classifier's turn cap: %v", err)
	}
}

// An unscoped author's spend must not be charged to some other scope —
// the failure mode a "last seen specialist" attribution would have.
func TestUnscopedSpendIsNotChargedToAScope(t *testing.T) {
	m := New(Config{
		Limits: Limits{RatePer1K: 1.0},
		Scopes: map[string]Limits{"analyst": {MaxCostUSD: 1.00}},
	})
	for i := 0; i < 20; i++ {
		if err := m.Observe(authoredEvent("synthesizer", 1000)); err != nil {
			t.Fatalf("synthesizer call %d tripped the analyst's cap: %v", i, err)
		}
	}
	tokens, cost, calls, ok := m.ScopeSnapshot("analyst")
	if !ok {
		t.Fatal("analyst has a scope; ScopeSnapshot should find it")
	}
	if tokens != 0 || cost != 0 || calls != 0 {
		t.Errorf("analyst spent nothing but the meter recorded %d tokens / $%v / %d calls", tokens, cost, calls)
	}
	if _, _, sessionCalls := m.Snapshot(); sessionCalls != 20 {
		t.Errorf("session calls = %d, want 20 — unscoped spend still meters against the workload", sessionCalls)
	}
}

// A scope's own rate prices its own tokens, and the session total is
// the sum of differently-priced calls rather than one multiplication.
func TestScopeRatePricesItsOwnTokens(t *testing.T) {
	m := New(Config{
		Limits: Limits{RatePer1K: 1.0},
		Scopes: map[string]Limits{"analyst": {RatePer1K: 0.01}},
	})
	if err := m.Observe(authoredEvent("synthesizer", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := m.Observe(authoredEvent("analyst", 1000)); err != nil {
		t.Fatal(err)
	}
	_, cost, _, ok := m.ScopeSnapshot("analyst")
	if !ok {
		t.Fatal("analyst scope missing")
	}
	if math.Abs(cost-0.01) > 1e-9 {
		t.Errorf("analyst cost = $%v, want $0.01 at its own rate (not $1.00 at the parent's)", cost)
	}
	_, session, _ := m.Snapshot()
	if math.Abs(session-1.01) > 1e-9 {
		t.Errorf("session cost = $%v, want $1.01 — the sum of two differently-priced calls", session)
	}
}

// A scope with no rate of its own is priced at the session's. This is
// the un-tiered roster, and it must meter exactly as it did before
// scopes existed.
func TestScopeWithoutARateInheritsTheSessionRate(t *testing.T) {
	scoped := New(Config{
		Limits: Limits{RatePer1K: 0.05},
		Scopes: map[string]Limits{"analyst": {MaxTurns: 100}},
	})
	flat := NewMeter(Limits{RatePer1K: 0.05})
	for i := 0; i < 3; i++ {
		if err := scoped.Observe(authoredEvent("analyst", 1000)); err != nil {
			t.Fatal(err)
		}
		if err := flat.Observe(authoredEvent("analyst", 1000)); err != nil {
			t.Fatal(err)
		}
	}
	_, want, _ := flat.Snapshot()
	_, got, _ := scoped.Snapshot()
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("scoped session cost $%v != unscoped $%v", got, want)
	}
}

// When one event crosses both ceilings, the specialist's is reported:
// it is the more specific fact, and it is the one an operator has to
// act on.
func TestScopeCeilingIsReportedAheadOfTheSessionCeiling(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxCostUSD: 1.00, RatePer1K: 1.0},
		Scopes: map[string]Limits{"analyst": {MaxCostUSD: 1.00}},
	})
	err := m.Observe(authoredEvent("analyst", 2000))
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("want ErrExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "analyst") {
		t.Errorf("both ceilings were crossed; the specialist's should be the one named: %v", err)
	}
}

func TestScopeSnapshotUnknownAgent(t *testing.T) {
	m := New(Config{Scopes: map[string]Limits{"analyst": {MaxTurns: 1}}})
	if _, _, _, ok := m.ScopeSnapshot("nobody"); ok {
		t.Error("ScopeSnapshot reported a scope for an agent that has none")
	}
}

// The meter must not alias the caller's scope map: a caller that reuses
// one map across sessions cannot be allowed to mutate a live meter's
// ceilings.
func TestNewCopiesTheScopeMap(t *testing.T) {
	scopes := map[string]Limits{"analyst": {MaxTurns: 1}}
	m := New(Config{Scopes: scopes})
	delete(scopes, "analyst")
	scopes["other"] = Limits{MaxTurns: 1}
	if err := m.Observe(authoredEvent("analyst", 10)); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if err := m.Observe(authoredEvent("analyst", 10)); !errors.Is(err, ErrExceeded) {
		t.Fatalf("call 2: the meter lost its scope when the caller mutated the map: %v", err)
	}
}

// ---------------------------------------------------------------------
// Catalog pricing: each call costed against the model it was billed to.

// pricedEvent fakes a call the catalog can price: the model it was billed
// against, plus the input/output split and the cache-read subset.
func pricedEvent(modelVersion string, prompt, cached, out int32) *session.Event {
	return &session.Event{
		LLMResponse: model.LLMResponse{
			ModelVersion: modelVersion,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        prompt,
				CachedContentTokenCount: cached,
				CandidatesTokenCount:    out,
				TotalTokenCount:         prompt + out,
			},
		},
	}
}

func builtinCatalog(t *testing.T) *pricing.Catalog {
	t.Helper()
	c, err := pricing.NewCatalog(pricing.Options{})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return c
}

// The flat rate is the average of a model's input and output rates, so it
// overcharges an input-heavy session — and every long-context agent is
// input-heavy. A cost ceiling that wrong fires on the wrong sessions, which
// is the reason the catalog path exists.
func TestCatalogPricesAnInputHeavyCallFarBelowTheFlatRate(t *testing.T) {
	const prompt, out = 1_000_000, 1_000
	// (2 + 10) / 2 / 1000, the rate internal/compose derives for sonnet.
	flatLimits := Limits{RatePer1K: (2 + 10) / 2.0 / 1000}
	exactLimits := flatLimits
	exactLimits.Catalog = builtinCatalog(t)

	flat := NewMeter(flatLimits)
	exact := NewMeter(exactLimits)
	if err := flat.Observe(pricedEvent("claude-sonnet-5", prompt, 0, out)); err != nil {
		t.Fatalf("flat: %v", err)
	}
	if err := exact.Observe(pricedEvent("claude-sonnet-5", prompt, 0, out)); err != nil {
		t.Fatalf("exact: %v", err)
	}
	_, flatCost, _ := flat.Snapshot()
	_, exactCost, _ := exact.Snapshot()
	if want := 1*2.0 + 0.001*10; exactCost < want*0.99 || exactCost > want*1.01 {
		t.Errorf("exact cost = $%.4f, want ~$%.4f", exactCost, want)
	}
	if flatCost <= exactCost*2 {
		t.Errorf("flat $%.4f is not the large overcharge this exists to fix (exact $%.4f)", flatCost, exactCost)
	}
	if n := exact.Unpriced(); n != 0 {
		t.Errorf("Unpriced() = %d, want 0 — the model is in the builtin catalog", n)
	}
}

// Cached input is billed at a tenth of fresh input, and on a cache-warm agent
// it is the majority of the prompt. TotalTokenCount cannot see that subset at
// all, so the flat path charges the same for both of these calls.
func TestCatalogBillsCacheReadsAtTheCacheRate(t *testing.T) {
	cold := NewMeter(Limits{Catalog: builtinCatalog(t)})
	warm := NewMeter(Limits{Catalog: builtinCatalog(t)})
	if err := cold.Observe(pricedEvent("claude-sonnet-5", 1_000_000, 0, 0)); err != nil {
		t.Fatalf("cold: %v", err)
	}
	if err := warm.Observe(pricedEvent("claude-sonnet-5", 1_000_000, 1_000_000, 0)); err != nil {
		t.Fatalf("warm: %v", err)
	}
	_, coldCost, _ := cold.Snapshot()
	_, warmCost, _ := warm.Snapshot()
	if warmCost >= coldCost {
		t.Fatalf("a fully cache-served prompt cost $%.4f, no less than a fresh one at $%.4f", warmCost, coldCost)
	}
}

// A catalog miss must fall back to the flat rate rather than silently
// dropping the call's cost to zero — a budget that stops metering is worse
// than one that meters approximately — and it has to be countable, or the
// caller cannot tell an exact figure from a mixed one.
func TestAnUnknownModelFallsBackToTheFlatRateAndIsCounted(t *testing.T) {
	m := NewMeter(Limits{Catalog: builtinCatalog(t), RatePer1K: 0.05})
	if err := m.Observe(pricedEvent("some-model-nobody-priced", 1000, 0, 0)); err != nil {
		t.Fatalf("observe: %v", err)
	}
	_, cost, _ := m.Snapshot()
	if want := 0.05; cost != want {
		t.Errorf("cost = $%.4f, want the flat-rate $%.4f", cost, want)
	}
	if n := m.Unpriced(); n != 1 {
		t.Errorf("Unpriced() = %d, want 1", n)
	}
}

// A provider that over-reports the cached counter must not make a call
// cheaper than the same call fully cached.
//
// core-agent's usage tracker clamps the same counter for the same reason, and
// the failure mode is one-directional: unclamped, the uncached half goes
// negative and CostUSDWithCache bills it at the input rate as a credit, so the
// more the provider miscounts the further away the ceiling gets. A ceiling
// that loosens under bad data is not a ceiling.
func TestAnOverReportedCacheCounterCannotCreditTheSession(t *testing.T) {
	bogus := NewMeter(Limits{Catalog: builtinCatalog(t)})
	honest := NewMeter(Limits{Catalog: builtinCatalog(t)})
	if err := bogus.Observe(pricedEvent("claude-sonnet-5", 1_000_000, 1_500_000, 1_000)); err != nil {
		t.Fatalf("bogus: %v", err)
	}
	if err := honest.Observe(pricedEvent("claude-sonnet-5", 1_000_000, 1_000_000, 1_000)); err != nil {
		t.Fatalf("honest: %v", err)
	}
	_, bogusCost, _ := bogus.Snapshot()
	_, honestCost, _ := honest.Snapshot()
	if bogusCost <= 0 {
		t.Fatalf("cost = $%.4f, want positive — a miscount is not a refund", bogusCost)
	}
	if bogusCost != honestCost {
		t.Errorf("cost = $%.4f, want $%.4f (clamped to a fully-cached prompt)", bogusCost, honestCost)
	}
}

// Scopes and the catalog compose: a specialist's own spend is priced
// against the model *it* ran on, not against the session's.
//
// This is the pairing the two features only have together, and it is the
// reason a per-specialist ceiling is worth anything on a tiered roster. A
// flat session rate charges a haiku subagent at the coordinator's price,
// so a cheap tier's ceiling trips at the wrong point — which is precisely
// the check a tiered roster exists to make.
func TestAScopedSpecialistIsPricedAgainstItsOwnModel(t *testing.T) {
	cat := builtinCatalog(t)
	m := New(Config{
		Limits: Limits{Catalog: cat},
		Scopes: map[string]Limits{
			"coordinator": {Catalog: cat},
			"analyst":     {Catalog: cat},
		},
	})

	// The same prompt and the same output, on the two tiers. Sonnet is
	// $2/$10 per MTok and haiku $1/$5, so the coordinator's call must
	// cost exactly twice the analyst's.
	big := pricedEvent("claude-sonnet-5", 1_000_000, 0, 100_000)
	big.Author = "coordinator"
	small := pricedEvent("claude-haiku-4-5", 1_000_000, 0, 100_000)
	small.Author = "analyst"
	if err := m.Observe(big); err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	if err := m.Observe(small); err != nil {
		t.Fatalf("analyst: %v", err)
	}

	_, coord, _, ok := m.ScopeSnapshot("coordinator")
	if !ok {
		t.Fatal("no scope recorded for the coordinator")
	}
	_, analyst, _, ok := m.ScopeSnapshot("analyst")
	if !ok {
		t.Fatal("no scope recorded for the analyst")
	}
	if want := 2*1.0 + 0.1*10; coord < want*0.99 || coord > want*1.01 {
		t.Errorf("coordinator spend = $%.4f, want ~$%.4f (sonnet rates)", coord, want)
	}
	if want := 1*1.0 + 0.1*5; analyst < want*0.99 || analyst > want*1.01 {
		t.Errorf("analyst spend = $%.4f, want ~$%.4f (haiku rates)", analyst, want)
	}
	if coord <= analyst {
		t.Errorf("the two tiers priced the same call alike: $%.4f vs $%.4f", coord, analyst)
	}

	// And the session total is the sum of the differently-priced calls,
	// not one multiplication over the combined token count.
	_, total, calls := m.Snapshot()
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if want := coord + analyst; total < want*0.99 || total > want*1.01 {
		t.Errorf("session total = $%.4f, want the sum of its scopes $%.4f", total, want)
	}
}

// A scope with no catalog of its own inherits the session's, the same
// way a scope with no rate inherits the session's rate. Without this an
// un-catalogued specialist would silently fall back to the flat rate and
// its ceiling would be the only one in the meter measured differently.
func TestAScopeInheritsTheSessionCatalog(t *testing.T) {
	m := New(Config{
		Limits: Limits{Catalog: builtinCatalog(t)},
		Scopes: map[string]Limits{"analyst": {MaxCostUSD: 100}},
	})
	ev := pricedEvent("claude-sonnet-5", 1_000_000, 0, 0)
	ev.Author = "analyst"
	if err := m.Observe(ev); err != nil {
		t.Fatalf("observe: %v", err)
	}
	_, spend, _, ok := m.ScopeSnapshot("analyst")
	if !ok {
		t.Fatal("no scope recorded")
	}
	if want := 2.0; spend < want*0.99 || spend > want*1.01 {
		t.Errorf("scope spend = $%.4f, want ~$%.4f — the session catalog was not inherited", spend, want)
	}
	if n := m.Unpriced(); n != 0 {
		t.Errorf("Unpriced() = %d, want 0", n)
	}
}
