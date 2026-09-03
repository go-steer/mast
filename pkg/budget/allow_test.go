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
	"strings"
	"testing"
)

// The claim Allow exists to make, on the one dimension where it can be
// proved without arithmetic about money: a turn cap enforced only by
// Observe is a turn cap plus one.
//
// crossed reports calls > MaxTurns, so a meter capped at 3 reports the
// crossing on the 4th call — after the 4th call has been paid for.
// There is no way to write that check later and have it fire earlier.
// Allow answers the question one call sooner and gets it exactly right,
// which is the entire argument for a pre-call seam.
func TestTheTurnCapIsExactBeforeTheCallAndLateAfterIt(t *testing.T) {
	const cap = 3

	observeOnly := NewMeter(Limits{MaxTurns: cap})
	calls := 0
	for {
		if err := observeOnly.Observe(usageEvent(10)); err != nil {
			break
		}
		calls++
		if calls > 10 {
			t.Fatal("the meter never reported a crossing")
		}
	}
	if _, _, paid := observeOnly.Snapshot(); paid != cap+1 {
		t.Errorf("post-hoc only: the meter folded %d calls under a cap of %d, want %d — "+
			"if this is now equal to the cap, Observe has become pre-call and Allow is redundant",
			paid, cap, cap+1)
	}

	// The same cap, asked before each call. The overshoot is gone, and
	// nothing about the ceiling changed.
	gated := NewMeter(Limits{MaxTurns: cap})
	made := 0
	for i := 0; i < 10; i++ {
		if err := gated.Allow(""); err != nil {
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("Allow returned %v, want an ErrRefused", err)
			}
			break
		}
		made++
		if err := gated.Observe(usageEvent(10)); err != nil {
			t.Fatalf("a call Allow permitted crossed the ceiling: %v", err)
		}
	}
	if made != cap {
		t.Errorf("pre-call: the workload made %d calls under a cap of %d, want %d", made, cap, cap)
	}
}

func TestAllowRefusesWhatCannotFitAndPermitsWhatMight(t *testing.T) {
	for _, tc := range []struct {
		name string
		lim  Limits
		// spend is applied before the question is asked.
		events    []int32
		wantRefus bool
		wantWord  string
	}{{
		name: "turns: room for one more",
		lim:  Limits{MaxTurns: 2}, events: []int32{10},
	}, {
		name: "turns: the next call would be one too many",
		lim:  Limits{MaxTurns: 2}, events: []int32{10, 10},
		wantRefus: true, wantWord: "turn) 3 of a cap of 2",
	}, {
		name: "tokens: under the cap, and the next call might still fit",
		lim:  Limits{MaxTokens: 100}, events: []int32{99},
	}, {
		name: "tokens: exactly at the cap, so nothing further can fit",
		lim:  Limits{MaxTokens: 100}, events: []int32{100},
		wantRefus: true, wantWord: "already at cap 100",
	}, {
		name: "cost: at the cap to the cent",
		// 1000 tokens at $1.00/1K is exactly $1.00.
		lim:       Limits{MaxCostUSD: 1.0, RatePer1K: 1.0},
		events:    []int32{1000},
		wantRefus: true, wantWord: "already at cap $1.0000",
	}, {
		name: "cost: a cent short, so the next call is the meter's business and not Allow's",
		lim:  Limits{MaxCostUSD: 1.0, RatePer1K: 1.0}, events: []int32{990},
	}, {
		name: "no ceilings: nothing to prove and nothing refused",
		lim:  Limits{}, events: []int32{1e6, 1e6, 1e6},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMeter(tc.lim)
			for _, tok := range tc.events {
				// Observe's own verdict is not the subject here; a
				// fixture may deliberately sit past a cap.
				_ = m.Observe(usageEvent(tok))
			}
			err := m.Allow("")
			if gotRefus := err != nil; gotRefus != tc.wantRefus {
				t.Fatalf("Allow = %v, want refused = %v", err, tc.wantRefus)
			}
			if !tc.wantRefus {
				return
			}
			if !errors.Is(err, ErrRefused) {
				t.Errorf("refusal is not an ErrRefused: %v", err)
			}
			if errors.Is(err, ErrExceeded) {
				t.Error("a refusal reported itself as an overshoot; " +
					"ErrExceeded means money was spent and no money was spent here")
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("refusal %q does not show the arithmetic (%q)", err, tc.wantWord)
			}
		})
	}
}

// The relationship Allow's correctness rests on, asserted rather than
// assumed from precluded and crossed having been written next to each
// other: whenever Allow refuses, the call it refused really would have
// crossed a ceiling. A refusal that fails this is a workload stopped
// short of a budget it had.
//
// The converse is deliberately NOT asserted. Allow permitting a call
// that turns out to cross is the expected case — it is what Observe is
// for, and the alternative is guessing at the size of a call that has
// not been made.
func TestARefusedCallReallyWouldHaveCrossed(t *testing.T) {
	limits := []Limits{
		{MaxTurns: 1}, {MaxTurns: 4},
		{MaxTokens: 50}, {MaxTokens: 1000},
		{MaxCostUSD: 0.5, RatePer1K: 1.0}, {MaxCostUSD: 5, RatePer1K: 1.0},
		{MaxTurns: 3, MaxTokens: 500, MaxCostUSD: 2, RatePer1K: 1.0},
	}
	// The smallest call worth calling a call. If Allow refuses, even
	// this must cross — anything larger crosses by more.
	const smallest = 1

	for _, l := range limits {
		for prior := 0; prior < 8; prior++ {
			m := NewMeter(l)
			for i := 0; i < prior; i++ {
				_ = m.Observe(usageEvent(100))
			}
			if m.Allow("") == nil {
				continue
			}
			// Refused. Prove the refusal was earned by making the call
			// on an identical meter and requiring Observe to report it.
			shadow := NewMeter(l)
			for i := 0; i < prior; i++ {
				_ = shadow.Observe(usageEvent(100))
			}
			if err := shadow.Observe(usageEvent(smallest)); !errors.Is(err, ErrExceeded) {
				t.Errorf("limits %+v after %d calls: Allow refused, but the refused call "+
					"did not cross anything (Observe said %v) — a workload was stopped "+
					"short of a budget it had", l, prior, err)
			}
		}
	}
}

func TestARefusalNamesTheTighterOfTheTwoCeilings(t *testing.T) {
	m := New(Config{
		Limits: Limits{MaxTurns: 100},
		Scopes: map[string]Limits{"noisy": {MaxTurns: 1}},
	})
	if err := m.Observe(authoredEvent("noisy", 10)); err != nil {
		t.Fatalf("first call: %v", err)
	}

	err := m.Allow("noisy")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("the specialist's own cap did not refuse: %v", err)
	}
	if !strings.Contains(err.Error(), `specialist "noisy"`) {
		t.Errorf("refusal %q does not name the specialist whose ceiling it is; "+
			"an operator raising the wrong cap buys nothing", err)
	}

	// The session is nowhere near its cap, so every other agent — and
	// the root — carries on. A specialist's ceiling is not the
	// workload's.
	if err := m.Allow("quiet"); err != nil {
		t.Errorf("an unscoped agent was refused by another agent's ceiling: %v", err)
	}
	if err := m.Allow(""); err != nil {
		t.Errorf("the root was refused by a specialist's ceiling: %v", err)
	}
}

// Precluded reports in Trip shape so an operator surface renders a
// refusal the way it renders an overshoot, and so a reset knows which
// dimension to raise.
func TestPrecludedReportsEveryDimensionThatBlocks(t *testing.T) {
	m := New(Config{Scopes: map[string]Limits{
		"sp": {MaxTurns: 1, MaxTokens: 10, MaxCostUSD: 0.001, RatePer1K: 1.0},
	}})
	_ = m.Observe(authoredEvent("sp", 100))

	got := map[string]bool{}
	for _, tr := range m.Precluded("sp") {
		if tr.Scope != "sp" {
			t.Errorf("trip %+v is not attributed to the scope that blocks", tr)
		}
		got[tr.Dimension] = true
	}
	for _, d := range []string{DimensionTurns, DimensionTokens, DimensionCostUSD} {
		if !got[d] {
			t.Errorf("dimension %q is past its cap but is not in Precluded: %+v", d, m.Precluded("sp"))
		}
	}
}

// A nil meter is a host saying "this call is not metered", which is not
// the same statement as a meter with no ceilings, and neither one
// refuses. The distinction matters at the wiring: a pre-call seam
// reached on an unmetered path must not fail closed and stop the
// workload, and must not pretend it enforced anything either.
func TestANilMeterRefusesNothing(t *testing.T) {
	var m *Meter
	if err := m.Allow("sp"); err != nil {
		t.Errorf("a nil meter refused a call: %v", err)
	}
	if tr := m.Precluded("sp"); tr != nil {
		t.Errorf("a nil meter reported trips: %+v", tr)
	}
}

// Grant is the operator's unwedge, and it has to clear a refusal for
// the same reason it clears a trip — otherwise raising the ceiling buys
// the runway on paper and the next call is still refused.
func TestRaisingTheCeilingClearsARefusal(t *testing.T) {
	m := NewMeter(Limits{MaxTurns: 1})
	_ = m.Observe(usageEvent(10))
	if err := m.Allow(""); err == nil {
		t.Fatal("the cap did not refuse, so this test proves nothing")
	}
	if _, err := m.Grant("", Limits{MaxTurns: 2}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := m.Allow(""); err != nil {
		t.Errorf("still refused after the ceiling was raised: %v", err)
	}
}
