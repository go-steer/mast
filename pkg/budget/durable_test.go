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
	"sync"
	"testing"

	"google.golang.org/adk/v2/session"
)

// The write half: every priced call is handed to the hook exactly once,
// with the author the ledger has to key on.
func TestOnSpendSeesEveryPricedCall(t *testing.T) {
	var got []Spend
	m := New(Config{
		Limits: Limits{RatePer1K: 0.001},
		Scopes: map[string]Limits{"analyst": {RatePer1K: 0.01}},
		OnSpend: func(s Spend) {
			got = append(got, s)
		},
	})

	_ = m.Observe(authoredEvent("coordinator", 1_000))
	_ = m.Observe(authoredEvent("analyst", 1_000))
	// Free events are not calls: a function response has no usage to
	// record, and a row for it would inflate the restored call count that
	// max_turns is metered against.
	_ = m.Observe(&session.Event{Author: "coordinator"})
	_ = m.Observe(nil)

	if len(got) != 2 {
		t.Fatalf("hook saw %d spends, want 2 (only the events carrying usage)", len(got))
	}
	if got[0].Author != "coordinator" || got[0].Tokens != 1_000 || math.Abs(got[0].CostUSD-0.001) > 1e-12 {
		t.Errorf("spend 0 = %+v, want coordinator / 1000 tokens / $0.001", got[0])
	}
	// The scope's own rate, because that is what the session was charged
	// and a ledger that re-derived it later could not know the roster.
	if got[1].Author != "analyst" || math.Abs(got[1].CostUSD-0.01) > 1e-12 {
		t.Errorf("spend 1 = %+v, want analyst priced at its own $0.01/1K", got[1])
	}
}

// The call that trips the ceiling is the most important row in the
// ledger: it is the one an operator's grant will be measured against,
// and dropping it would understate exactly the sessions this exists for.
func TestOnSpendFiresForTheCallThatCrossedTheCeiling(t *testing.T) {
	var got []Spend
	m := New(Config{
		Limits:  Limits{MaxCostUSD: 0.01, RatePer1K: 0.001},
		OnSpend: func(s Spend) { got = append(got, s) },
	})

	// 20k tokens at $0.001/1K is $0.02, over the $0.01 cap in one call.
	if err := m.Observe(usageEvent(20_000)); !errors.Is(err, ErrExceeded) {
		t.Fatalf("Observe = %v, want ErrExceeded", err)
	}
	if len(got) != 1 {
		t.Fatalf("hook saw %d spends, want the call that crossed the ceiling", len(got))
	}
	if math.Abs(got[0].CostUSD-0.02) > 1e-12 {
		t.Errorf("spend = $%.4f, want the $0.0200 the call actually cost", got[0].CostUSD)
	}
}

// A hook that read the meter back would deadlock if it ran under the
// fold's lock. It does not, and this test would hang rather than fail if
// that ever changed — which is the honest failure mode for a lock bug.
func TestOnSpendRunsOutsideTheLock(t *testing.T) {
	var tokens int64
	m := New(Config{Limits: Limits{RatePer1K: 0.001}})
	m.onSpend = func(s Spend) {
		tokens, _, _ = m.Snapshot()
	}
	_ = m.Observe(usageEvent(500))
	if tokens != 500 {
		t.Fatalf("the hook read %d tokens back from the meter, want 500", tokens)
	}
}

// A call the catalog could not price is marked, so a restored total can
// be labelled the same mixed-model approximation a live one is.
func TestOnSpendMarksUnpricedCalls(t *testing.T) {
	var got Spend
	m := New(Config{
		Limits:  Limits{Catalog: builtinCatalog(t), RatePer1K: 0.001},
		OnSpend: func(s Spend) { got = s },
	})
	_ = m.Observe(pricedEvent("no-such-model-9000", 1_000, 0, 100))
	if !got.Unpriced {
		t.Errorf("spend = %+v, want Unpriced for a model the catalog does not carry", got)
	}
}

// The read half. Restore adds prior spend to the session and to each
// scope the current config still carries.
func TestRestoreSeedsSessionAndScopes(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxCostUSD: 1.00, MaxTokens: 10_000, MaxTurns: 5, RatePer1K: 0.001},
		Scopes: map[string]Limits{"analyst": {MaxCostUSD: 0.50}},
	})
	err := m.Restore(Prior{
		Session:  Totals{Tokens: 4_000, CostUSD: 0.40, Calls: 3},
		ByAuthor: map[string]Totals{"analyst": {Tokens: 1_000, CostUSD: 0.10, Calls: 1}},
		Unpriced: 2,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	tokens, cost, calls := m.Snapshot()
	if tokens != 4_000 || calls != 3 || math.Abs(cost-0.40) > 1e-12 {
		t.Errorf("session = %d tokens / $%.4f / %d calls, want 4000 / $0.4000 / 3", tokens, cost, calls)
	}
	st, sc, scalls, ok := m.ScopeSnapshot("analyst")
	if !ok || st != 1_000 || scalls != 1 || math.Abs(sc-0.10) > 1e-12 {
		t.Errorf("analyst = %d tokens / $%.4f / %d calls (ok=%v), want 1000 / $0.1000 / 1", st, sc, scalls, ok)
	}
	if m.Unpriced() != 2 {
		t.Errorf("Unpriced = %d, want the 2 restored", m.Unpriced())
	}
	if !m.Restored() {
		t.Error("Restored() is false after a successful Restore")
	}
}

// Restoring adds to what this process has already metered rather than
// assigning over it: a caller whose first read failed and who retried a
// turn later has real spend in both places, and they sum.
func TestRestoreAddsToLiveSpend(t *testing.T) {
	m := New(Config{Limits: Limits{RatePer1K: 0.001}})
	_ = m.Observe(usageEvent(1_000)) // $0.001 in this process

	if err := m.Restore(Prior{Session: Totals{Tokens: 2_000, CostUSD: 0.002, Calls: 1}}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	tokens, cost, calls := m.Snapshot()
	if tokens != 3_000 || calls != 2 || math.Abs(cost-0.003) > 1e-12 {
		t.Fatalf("after Restore: %d tokens / $%.4f / %d calls, want 3000 / $0.0030 / 2", tokens, cost, calls)
	}
}

// Twice is an error, not a no-op. Prior spend is additive, so a second
// fold doubles it — and the sessions this matters for are the ones
// already near a ceiling, where the double count decides the turn.
func TestRestoreTwiceIsRefused(t *testing.T) {
	m := New(Config{Limits: Limits{RatePer1K: 0.001}})
	p := Prior{Session: Totals{Tokens: 1_000, CostUSD: 0.001, Calls: 1}}
	if err := m.Restore(p); err != nil {
		t.Fatalf("first Restore: %v", err)
	}
	if err := m.Restore(p); !errors.Is(err, ErrRestored) {
		t.Fatalf("second Restore = %v, want ErrRestored", err)
	}
	if _, cost, _ := m.Snapshot(); math.Abs(cost-0.001) > 1e-12 {
		t.Fatalf("cost after a refused second Restore = $%.4f, want $0.0010 (it must not have been applied)", cost)
	}
}

// Restore is the accumulator catching up with the facts, not an
// enforcement point: a meter restored past its cap reports it through
// Trips and stops the next call through Observe, by the same comparison
// a meter that never restarted would use. There is no trip flag to set
// (see trips.go), which is what makes this safe.
func TestRestorePastTheCeilingReportsRatherThanErrors(t *testing.T) {
	m := New(Config{Limits: Limits{MaxCostUSD: 0.01, RatePer1K: 0.001}})
	if err := m.Restore(Prior{Session: Totals{Tokens: 20_000, CostUSD: 0.02, Calls: 1}}); err != nil {
		t.Fatalf("Restore over the ceiling returned %v; restoring is not an enforcement point", err)
	}
	trips := m.Trips()
	if len(trips) != 1 || trips[0].Dimension != DimensionCostUSD {
		t.Fatalf("Trips() = %+v, want one cost trip", trips)
	}
	if err := m.Observe(usageEvent(1)); !errors.Is(err, ErrExceeded) {
		t.Fatalf("the next call after an over-ceiling restore = %v, want ErrExceeded", err)
	}
}

// A roster edit between processes is normal — a specialist can be added
// or removed. Spend by an author the current config carries no scope for
// still counts against the session; there is simply no scope ceiling
// left to meter it against.
func TestRestoreIgnoresAnAuthorWithNoScope(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxCostUSD: 1.00, RatePer1K: 0.001},
		Scopes: map[string]Limits{"analyst": {MaxCostUSD: 0.50}},
	})
	err := m.Restore(Prior{
		// Session total covers both authors; only one is still scoped.
		Session: Totals{Tokens: 3_000, CostUSD: 0.30, Calls: 2},
		ByAuthor: map[string]Totals{
			"analyst": {Tokens: 1_000, CostUSD: 0.10, Calls: 1},
			"retired": {Tokens: 2_000, CostUSD: 0.20, Calls: 1},
		},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, cost, _ := m.Snapshot(); math.Abs(cost-0.30) > 1e-12 {
		t.Errorf("session cost = $%.4f, want the whole $0.3000 including the retired specialist's", cost)
	}
	if _, _, _, ok := m.ScopeSnapshot("retired"); ok {
		t.Error("Restore invented a scope for an author the config does not carry")
	}
	if _, cost, _, _ := m.ScopeSnapshot("analyst"); math.Abs(cost-0.10) > 1e-12 {
		t.Errorf("analyst cost = $%.4f, want $0.1000", cost)
	}
}

// The common case: a session nobody has ever metered. IsZero is what
// lets a caller skip the write and the latch.
func TestPriorIsZero(t *testing.T) {
	if !(Prior{}).IsZero() {
		t.Error("the zero Prior does not report IsZero")
	}
	cases := map[string]Prior{
		"session":  {Session: Totals{Calls: 1}},
		"byAuthor": {ByAuthor: map[string]Totals{"analyst": {}}},
		"unpriced": {Unpriced: 1},
	}
	for name, p := range cases {
		if p.IsZero() {
			t.Errorf("Prior with %s set reports IsZero", name)
		}
	}
}

// A meter with no hook is the library-embed case: no ledger, no
// callback, exactly the pre-#175 behavior.
func TestNoHookIsInert(t *testing.T) {
	m := NewMeter(Limits{RatePer1K: 0.001})
	if err := m.Observe(usageEvent(1_000)); err != nil {
		t.Fatalf("Observe with no hook: %v", err)
	}
	if m.Restored() {
		t.Error("a meter nobody restored reports Restored()")
	}
}

// Observe is called from the event stream and the hook writes a row per
// call; both have to hold up when a coordinator and its specialists
// stream concurrently.
func TestOnSpendUnderConcurrentObserve(t *testing.T) {
	var mu sync.Mutex
	var seen int
	m := New(Config{
		Limits: Limits{RatePer1K: 0.001},
		OnSpend: func(Spend) {
			mu.Lock()
			seen++
			mu.Unlock()
		},
	})

	const goroutines, each = 8, 10
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				_ = m.Observe(usageEvent(100))
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if seen != goroutines*each {
		t.Errorf("hook fired %d times, want %d — one row per call or the ledger is short", seen, goroutines*each)
	}
	if tokens, _, calls := m.Snapshot(); tokens != goroutines*each*100 || calls != goroutines*each {
		t.Errorf("meter = %d tokens / %d calls, want %d / %d", tokens, calls, goroutines*each*100, goroutines*each)
	}
}

func TestTotalsString(t *testing.T) {
	got := Totals{Tokens: 1_200, CostUSD: 0.5, Calls: 3}.String()
	want := "$0.5000 over 3 calls (1200 tokens)"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
