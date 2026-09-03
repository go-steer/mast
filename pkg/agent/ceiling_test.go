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

// The W10.2 acceptance test: a real budget.Meter, a real runner, and the
// daemon's own turn loop.
//
// precall_test.go proves the seam works against a stub gate; allow_test.go
// proves the arithmetic. Neither proves the thing the plan asked for,
// which is a property of the two halves wired together — that a workload
// standing at its ceiling makes no further model call, counted at the
// provider and read off the meter rather than inferred from a transcript.
//
// Every test here runs the same workload twice: once with the gate
// installed and once without. The ungated run is not a control in the
// usual sense of proving the fixture reaches the code under test. It is
// mast's behaviour through v0.5 — the post-hoc fold alone — so the two
// columns of each table are the before and after of this change, and the
// gap between them is the call the ceiling used to cost.
package agent_test

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
)

// meteredModel is countingModel that also reports token usage, so a real
// meter has something to fold. Every call reports the same size, which
// makes the arithmetic in these tests checkable by hand.
type meteredModel struct {
	name   string
	tokens int32

	mu    sync.Mutex
	calls int
}

func (m *meteredModel) Name() string { return m.name }

func (m *meteredModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {
		resp := &model.LLMResponse{
			Content: genai.NewContentFromText("answered", genai.RoleModel),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				TotalTokenCount: m.tokens,
			},
		}
		yield(resp, nil)
	}
}

func (m *meteredModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// ledger records what the meter's durability seam was told to persist
// (#175). A refused call must leave no entry here: the spend a workload
// is billed for after the fact has to match the calls it actually made,
// or an operator reconciling a bill against a transcript finds money
// that bought nothing.
type ledger struct {
	mu     sync.Mutex
	spends []budget.Spend
}

func (l *ledger) record(s budget.Spend) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spends = append(l.spends, s)
}

func (l *ledger) entries() []budget.Spend {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]budget.Spend(nil), l.spends...)
}

func (l *ledger) total() float64 {
	var sum float64
	for _, s := range l.entries() {
		sum += s.CostUSD
	}
	return sum
}

// meteredTurn is the daemon's turn loop in miniature: install the gate,
// stream the run, fold every event, stop on a crossed ceiling. The
// shape is copied from cmd/mast/main.go's runTurn deliberately — a
// harness that folded differently from production would be testing
// itself.
//
// gate is passed separately from meter so a caller can run the identical
// workload with the post-hoc fold alone.
func meteredTurn(t *testing.T, root adkagent.Agent, meter *budget.Meter, gate mastagent.CallGate, sessionID string) ([]*session.Event, error) {
	t.Helper()
	// A fresh service per turn, which is closer to the daemon than one
	// long conversation would be: a session there survives the process,
	// and the meter is the thing that carries spend across turns.
	r, err := runner.New(runner.Config{
		AppName:           "ceiling-test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	ctx := mastagent.WithCallGate(context.Background(), gate)
	var out []*session.Event
	for ev, err := range r.Run(ctx, "user", sessionID,
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		out = append(out, ev)
		if berr := meter.Observe(ev); berr != nil {
			return out, berr
		}
	}
	return out, nil
}

// The plan's acceptance test, on the dimension where the post-hoc fold
// is not merely late but cannot be right: turns.
//
// crossed() reports MaxTurns with >, so a 3-turn cap is reported crossed
// on call 4 — the cap has always been paid for by exceeding it. Standing
// at 3 of 3, Observe would permit a fourth call and then abort the turn
// it had already bought. Allow refuses it, and the assertion is at the
// provider: zero further calls.
func TestAWorkloadAtItsTurnCapMakesNoFurtherModelCall(t *testing.T) {
	const turnCap = 3

	for _, tc := range []struct {
		name  string
		gated bool

		wantExtraCalls int
		wantMeterCalls int
		wantLedger     int
	}{
		// v0.5's behaviour, kept as the comparison: the fourth call is
		// made, priced, written to the ledger and only then reported.
		{name: "post-hoc fold alone", gated: false,
			wantExtraCalls: 1, wantMeterCalls: turnCap + 1, wantLedger: turnCap + 1},
		{name: "with the pre-call gate", gated: true,
			wantExtraCalls: 0, wantMeterCalls: turnCap, wantLedger: turnCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			led := &ledger{}
			meter := budget.New(budget.Config{
				Limits:  budget.Limits{MaxTurns: turnCap, RatePer1K: 1.0},
				OnSpend: led.record,
			})
			m := &meteredModel{name: "m", tokens: 1000}

			// Spend the cap, one turn at a time. None of these may trip:
			// a workload that cannot reach its own ceiling is not
			// measuring the ceiling.
			for i := 1; i <= turnCap; i++ {
				if _, err := meteredTurn(t, coordinator(t, "root", m), meter, gateFor(tc.gated, meter), "s"); err != nil {
					t.Fatalf("turn %d of %d tripped early: %v", i, turnCap, err)
				}
			}
			if _, _, calls := meter.Snapshot(); calls != turnCap {
				t.Fatalf("after %d turns the meter counted %d model calls, want %d", turnCap, calls, turnCap)
			}
			if got := m.count(); got != turnCap {
				t.Fatalf("the provider saw %d calls over %d turns, want %d", got, turnCap, turnCap)
			}
			atCap, costAtCap, _ := meter.Snapshot()

			// The turn the ceiling is about.
			events, err := meteredTurn(t, coordinator(t, "root", m), meter, gateFor(tc.gated, meter), "s")

			if got := m.count() - turnCap; got != tc.wantExtraCalls {
				t.Errorf("the provider was asked for %d more calls past the cap, want %d", got, tc.wantExtraCalls)
			}
			tokens, cost, calls := meter.Snapshot()
			if calls != tc.wantMeterCalls {
				t.Errorf("the meter counted %d model calls, want %d", calls, tc.wantMeterCalls)
			}
			if n := len(led.entries()); n != tc.wantLedger {
				t.Errorf("the durable ledger holds %d spends, want %d", n, tc.wantLedger)
			}
			// The ledger and the meter have to agree. A refusal that
			// wrote to one and not the other would reconcile wrong,
			// which is worse than either being late.
			if got, want := led.total(), cost; !nearly(got, want) {
				t.Errorf("ledger total $%.4f disagrees with the meter's $%.4f", got, want)
			}

			if tc.gated {
				if tokens != atCap || cost != costAtCap {
					t.Errorf("a refused call moved the meter: %d tokens/$%.4f, was %d/$%.4f",
						tokens, cost, atCap, costAtCap)
				}
				if err != nil {
					t.Errorf("the refused turn returned %v, want no error: nothing crossed, because nothing was spent", err)
				}
				if text := transcript(events); !mastagent.Refused(text) {
					t.Errorf("the refusal is not in the transcript an operator reads:\n%s", text)
				}
				return
			}
			// Ungated: the cap was enforced, but only by crossing it.
			if !errors.Is(err, budget.ErrExceeded) {
				t.Errorf("the post-hoc fold returned %v, want budget.ErrExceeded", err)
			}
			if cost <= costAtCap {
				t.Errorf("the overshooting call cost nothing ($%.4f, was $%.4f) — the fixture is not measuring spend", cost, costAtCap)
			}
		})
	}
}

// The same shape on cost, where the gap between the two predicates is
// the one Allow's >= opens: at exactly the cap the ceiling is met and
// not yet crossed, so the fold permits one more call.
//
// Four calls at $0.01 each land exactly on a $0.04 ceiling. Nothing here
// rounds: the rate is per 1K tokens and the calls are 1000 tokens.
func TestAWorkloadExactlyAtItsCostCapMakesNoFurtherModelCall(t *testing.T) {
	for _, gated := range []bool{false, true} {
		name := "post-hoc fold alone"
		if gated {
			name = "with the pre-call gate"
		}
		t.Run(name, func(t *testing.T) {
			led := &ledger{}
			meter := budget.New(budget.Config{
				Limits:  budget.Limits{MaxCostUSD: 0.04, RatePer1K: 0.01},
				OnSpend: led.record,
			})
			m := &meteredModel{name: "m", tokens: 1000}
			for i := 1; i <= 4; i++ {
				if _, err := meteredTurn(t, coordinator(t, "root", m), meter, gateFor(gated, meter), "s"); err != nil {
					t.Fatalf("turn %d tripped early: %v", i, err)
				}
			}
			if _, cost, _ := meter.Snapshot(); !nearly(cost, 0.04) {
				t.Fatalf("four calls cost $%.4f, want exactly the $0.0400 cap — the fixture is off", cost)
			}

			_, err := meteredTurn(t, coordinator(t, "root", m), meter, gateFor(gated, meter), "s")

			extra := m.count() - 4
			if gated {
				if extra != 0 {
					t.Errorf("a workload sitting on its cost cap made %d more calls, want 0", extra)
				}
				if _, cost, _ := meter.Snapshot(); !nearly(cost, 0.04) {
					t.Errorf("a refused call moved the cost to $%.4f, want it left at $0.0400", cost)
				}
				if n := len(led.entries()); n != 4 {
					t.Errorf("the durable ledger holds %d spends, want 4 — a refused call is phantom spend", n)
				}
				return
			}
			if extra != 1 {
				t.Errorf("the ungated workload made %d more calls, want 1 — this arm has to overshoot or it proves nothing", extra)
			}
			if !errors.Is(err, budget.ErrExceeded) {
				t.Errorf("the post-hoc fold returned %v, want budget.ErrExceeded", err)
			}
		})
	}
}

// The negative that keeps the two dimensions honest against each other:
// a workload with headroom on both is not refused, and the gate costs it
// nothing. Without this, a Precluded that returned every ceiling
// unconditionally would pass both tests above.
func TestAWorkloadWithHeadroomIsNotRefused(t *testing.T) {
	led := &ledger{}
	meter := budget.New(budget.Config{
		Limits:  budget.Limits{MaxTurns: 10, MaxCostUSD: 5, MaxTokens: 100_000, RatePer1K: 0.01},
		OnSpend: led.record,
	})
	m := &meteredModel{name: "m", tokens: 1000}
	for i := 1; i <= 5; i++ {
		if _, err := meteredTurn(t, coordinator(t, "root", m), meter, meter, "s"); err != nil {
			t.Fatalf("turn %d under a generous budget returned %v, want nil", i, err)
		}
	}
	if got := m.count(); got != 5 {
		t.Errorf("five gated turns reached the provider %d times, want 5", got)
	}
	if _, _, calls := meter.Snapshot(); calls != 5 {
		t.Errorf("the meter counted %d calls, want 5", calls)
	}
	if n := len(led.entries()); n != 5 {
		t.Errorf("the ledger holds %d spends, want 5", n)
	}
}

// An unlimited meter is still a gate, and still allows everything. This
// is the shape a host gets from a workload with no budget block: metered
// for reporting, ceilinged nowhere. Distinct from no gate at all, which
// TestAnUngatedTurnRunsUnchanged covers.
func TestAMeterWithNoCeilingsRefusesNothing(t *testing.T) {
	meter := budget.New(budget.Config{Limits: budget.Limits{RatePer1K: 0.01}})
	m := &meteredModel{name: "m", tokens: 1_000_000}
	for i := 1; i <= 3; i++ {
		if _, err := meteredTurn(t, coordinator(t, "root", m), meter, meter, "s"); err != nil {
			t.Fatalf("turn %d against an unlimited meter returned %v, want nil", i, err)
		}
	}
	if got := m.count(); got != 3 {
		t.Errorf("three turns reached the provider %d times, want 3", got)
	}
}

// gateFor is the switch between the two arms: the meter itself, or no
// gate at all. *budget.Meter satisfies mastagent.CallGate, which is the
// wiring under test as much as anything else here — the interface is
// declared in pkg/agent and never mentions pkg/budget.
func gateFor(gated bool, m *budget.Meter) mastagent.CallGate {
	if !gated {
		return nil
	}
	return m
}

// nearly compares money. Costs here accumulate by repeated float
// addition, so an exact compare against a hand-computed total is a
// flake waiting for a different token count.
func nearly(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
