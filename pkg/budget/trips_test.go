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
	"reflect"
	"testing"
)

// The premise the whole reset surface rests on: enforcement is
// re-derived every turn, so a session past its cap is not "tripped
// once" — it trips again on the next priced event, forever, until
// something raises the ceiling.
func TestTripPersistsUntilTheCeilingRises(t *testing.T) {
	m := NewMeter(Limits{MaxTurns: 2})
	for i := 0; i < 3; i++ {
		_ = m.Observe(usageEvent(10))
	}
	if len(m.Trips()) != 1 {
		t.Fatalf("trips = %v, want the turn cap crossed", m.Trips())
	}
	// A fresh turn against the same meter dies the same way.
	if err := m.Observe(usageEvent(10)); !errors.Is(err, ErrExceeded) {
		t.Fatalf("next turn: %v, want ErrExceeded — the trip is derived, not latched", err)
	}
	if _, err := m.Grant("", Limits{MaxTurns: 10}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if got := m.Trips(); len(got) != 0 {
		t.Fatalf("trips after grant = %v, want none", got)
	}
	if err := m.Observe(usageEvent(10)); err != nil {
		t.Fatalf("turn after grant: %v, want it to run", err)
	}
}

// Trips reports each crossed dimension, not just the first, because
// an operator granting runway needs to know every ceiling they have
// to clear — a $5 grant that leaves the turn cap crossed buys nothing.
func TestTripsReportEveryCrossedDimension(t *testing.T) {
	m := NewMeter(Limits{MaxTurns: 1, MaxTokens: 5, MaxCostUSD: 0.001, RatePer1K: 1})
	_ = m.Observe(usageEvent(10))
	_ = m.Observe(usageEvent(10))

	var dims []string
	for _, tr := range m.Trips() {
		dims = append(dims, tr.Dimension)
	}
	want := []string{DimensionTurns, DimensionTokens, DimensionCostUSD}
	if !reflect.DeepEqual(dims, want) {
		t.Errorf("dimensions = %v, want %v", dims, want)
	}
	for _, tr := range m.Trips() {
		if tr.Reason == "" {
			t.Errorf("%s trip carries no reason; the operator cannot size the grant", tr.Dimension)
		}
	}
}

// A specialist's own ceiling stops the same run the session's does, so
// it has to be reported — and attributed, or an operator raises the
// workload budget and watches the session wedge again.
func TestTripsAttributeTheScope(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxTokens: 10_000},
		Scopes: map[string]Limits{"log-analyst": {MaxTokens: 100}},
	})
	_ = m.Observe(authoredEvent("log-analyst", 150))

	trips := m.Trips()
	if len(trips) != 1 || trips[0].Scope != "log-analyst" {
		t.Fatalf("trips = %+v, want one attributed to log-analyst", trips)
	}
	// Raising the session's ceiling does not clear a scope's.
	if remaining := m.TripsAfter("", Limits{MaxTokens: 1_000_000}); len(remaining) != 1 {
		t.Errorf("after a session grant: %+v, want the specialist still over", remaining)
	}
	if remaining := m.TripsAfter("log-analyst", Limits{MaxTokens: 100}); len(remaining) != 0 {
		t.Errorf("after a scope grant: %+v, want cleared", remaining)
	}
}

// TripsAfter is the pre-flight check the reset runs, and it must not
// move anything: a refused 409 has to leave the ceilings exactly where
// it found them so the operator can re-issue with a bigger number.
func TestTripsAfterDoesNotMutate(t *testing.T) {
	m := NewMeter(Limits{MaxCostUSD: 0.01, RatePer1K: 1})
	_ = m.Observe(usageEvent(20)) // $0.02

	if remaining := m.TripsAfter("", Limits{MaxCostUSD: 0.005}); len(remaining) != 1 {
		t.Fatalf("a grant that doesn't cover the overspend should leave the trip: %+v", remaining)
	}
	if got := m.SessionLimits().MaxCostUSD; got != 0.01 {
		t.Errorf("ceiling = %v after a simulated grant, want the original 0.01", got)
	}
	if len(m.Trips()) != 1 {
		t.Errorf("trips changed after a simulated grant")
	}
}

// Adding budget must never be able to IMPOSE one. Handing 5 turns to a
// session with no turn cap would otherwise cap it at 5 — a reset that
// halts the session it was called to unwedge.
func TestGrantNeverImposesACeiling(t *testing.T) {
	m := NewMeter(Limits{MaxCostUSD: 1})
	got, err := m.Grant("", Limits{MaxCostUSD: 1, MaxTokens: 50_000, MaxTurns: 5})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if got.MaxCostUSD != 2 {
		t.Errorf("MaxCostUSD = %v, want the bounded dimension raised to 2", got.MaxCostUSD)
	}
	if got.MaxTokens != 0 || got.MaxTurns != 0 {
		t.Errorf("limits = %+v, want the unlimited dimensions left unlimited", got)
	}
}

// A typo'd specialist name is an error, not a silent no-op: the caller
// believes they just bought runway.
func TestGrantRejectsUnknownScope(t *testing.T) {
	m := New(Config{Scopes: map[string]Limits{"log-analyst": {MaxTurns: 3}}})
	if _, err := m.Grant("log-analist", Limits{MaxTurns: 3}); err == nil {
		t.Fatal("Grant to an unknown scope returned nil error")
	}
	if l, _ := m.ScopeLimits("log-analyst"); l.MaxTurns != 3 {
		t.Errorf("the real scope was modified: %+v", l)
	}
}

// The projection reads limits and scope names; both have to reflect
// grants, and scope order has to be stable or a client's list jumps
// between polls.
func TestLimitAccessorsReflectGrants(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxTurns: 2},
		Scopes: map[string]Limits{"zeta": {MaxTurns: 1}, "alpha": {MaxTurns: 1}},
	})
	if got := m.ScopeNames(); !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Errorf("ScopeNames = %v, want sorted", got)
	}
	if _, err := m.Grant("alpha", Limits{MaxTurns: 4}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if l, ok := m.ScopeLimits("alpha"); !ok || l.MaxTurns != 5 {
		t.Errorf("ScopeLimits(alpha) = %+v ok=%v, want MaxTurns 5", l, ok)
	}
	if _, ok := m.ScopeLimits("nobody"); ok {
		t.Error("ScopeLimits reported a scope that doesn't exist")
	}
}
