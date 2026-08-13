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
