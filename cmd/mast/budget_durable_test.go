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

// A ceiling that a restart resets is a ceiling on what a workload spends
// per process (#175). These tests simulate the restart the way the
// watchdog's do — a fresh meterPool over the same database — and assert
// the accumulator comes back with it.
package main

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	adksession "google.golang.org/adk/v2/session"
	"gorm.io/gorm"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/eventlog"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/watchdog"
	"github.com/go-steer/mast/pkg/workload"
)

// spendDB returns an on-disk SQLite connection and both stores over it.
// On-disk rather than ":memory:" for the reason the guardrail tests use
// it: the whole claim is that a second pool — standing in for the next
// process — reads what the first one wrote.
func spendDB(t *testing.T) (*gorm.DB, *eventlog.SpendStore, *eventlog.GuardrailStore) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "spend.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	ctx := context.Background()
	ledger, err := eventlog.NewSpendStore(ctx, db)
	if err != nil {
		t.Fatalf("NewSpendStore: %v", err)
	}
	guards, err := eventlog.NewGuardrailStore(ctx, db)
	if err != nil {
		t.Fatalf("NewGuardrailStore: %v", err)
	}
	return db, ledger, guards
}

// cappedBundle is the fixture ceiling: $1.00 a session, which at echo's
// inflated $0.05/1K is 20k tokens.
func cappedBundle() *workload.Bundle {
	b := &workload.Bundle{}
	b.Budget.MaxCostUSD = 1.00
	return b
}

// restartPool is one "process": a meterPool sized from the same bundle
// and roster, over the same database. Restarting means calling it again.
func restartPool(t *testing.T, ledger *eventlog.SpendStore, guards *eventlog.GuardrailStore, specs []specialists.Spec) *meterPool {
	t.Helper()
	mp := newMeterPool(cappedBundle(), specs, "", "echo")
	mp.durable(ledger, guards, discardLogger())
	return mp
}

// The headline. A workload stopped by max_cost_usd after $1.02 must not
// resume with $1.00 available, and a crash loop must not be able to
// spend the cap once per restart indefinitely.
func TestBudgetSpendSurvivesARestart(t *testing.T) {
	_, ledger, guards := spendDB(t)
	ctx := context.Background()
	const sid = "incident-durable"

	// Partial spend: 12k tokens at $0.05/1K is $0.60, well under the cap.
	first := restartPool(t, ledger, guards, nil)
	first.restore(ctx, sid)
	if err := first.meter(sid).Observe(spend12k()); err != nil {
		t.Fatalf("first spend was refused under a $1.00 cap: %v", err)
	}
	if _, cost, _ := first.meter(sid).Snapshot(); !sameUSD(cost, 0.60) {
		t.Fatalf("first process spent $%.4f, want $0.6000; the fixture is wrong", cost)
	}

	// The restart: a brand-new pool with no memory of any of it.
	second := restartPool(t, ledger, guards, nil)
	if _, cost, _ := second.meter(sid).Snapshot(); cost != 0 {
		t.Fatalf("the fresh pool started at $%.4f before it read anything", cost)
	}

	second.restore(ctx, sid)
	tokens, cost, calls := second.meter(sid).Snapshot()
	if !sameUSD(cost, 0.60) || tokens != 12_000 || calls != 1 {
		t.Fatalf("after restart: $%.4f over %d calls (%d tokens), want $0.6000 over 1 call (12000 tokens)", cost, calls, tokens)
	}

	// And the ceiling now bites where it would have without the restart:
	// another $0.60 is $1.20 against a $1.00 cap.
	err := second.meter(sid).Observe(spend12k())
	if !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("the restored meter allowed $1.20 against a $1.00 cap: %v", err)
	}
}

// The second half of the acceptance: a session already over its ceiling
// is still refused on the other side of a restart — and refused before
// the model is called, not after. A wedged session that buys one model
// call per turn forever is what durable spend would otherwise create,
// because nothing clears the accumulator any more.
func TestOverspentSessionIsRefusedAfterARestart(t *testing.T) {
	_, ledger, guards := spendDB(t)
	ctx := context.Background()
	const sid = "incident-overspent"

	first := restartPool(t, ledger, guards, nil)
	first.restore(ctx, sid)
	// 24k tokens is $1.20 against the $1.00 cap: over in one call.
	if err := first.meter(sid).Observe(spend24k()); !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("the fixture did not trip the ceiling: %v", err)
	}

	// The restart, driven through the real turn stack so the assertion is
	// about what the daemon does, not about what the pool knows.
	next := &loopingModel{rounds: 2}
	h := newTurnHarnessOpts(t, next, watchdog.ModeWarn, pokeTool(t))
	h.meters = restartPool(t, ledger, guards, nil)

	err := h.turn(ctx, sid)
	if err == nil {
		t.Fatal("a session $0.20 over its ceiling ran a turn after a restart")
	}
	if !errors.Is(err, inject.ErrConflict) {
		t.Fatalf("refusal err = %v, want inject.ErrConflict (409, the operator has to reset it)", err)
	}
	if got := next.calls.Load(); got != 0 {
		t.Errorf("the restored ceiling still called the model %d times; that is a ceiling that costs money after it trips", got)
	}
	if !strings.Contains(err.Error(), "guardrails/reset") {
		t.Errorf("refusal = %q, does not tell the operator how to clear it", err)
	}
	if !strings.Contains(err.Error(), "cap $1.0000") {
		t.Errorf("refusal = %q, does not say which ceiling was crossed", err)
	}
}

// The grant an operator hands over has to survive the restart too. #166
// recorded grants and did not replay them, because raising a ceiling
// over an accumulator that had forgotten what it spent is arithmetic on
// a number that no longer means anything. Restoring the spend inverts
// that: without the grant, a session the operator rescued at $1.20 comes
// back wedged by a restart they never made.
func TestOperatorGrantIsReplayedAfterARestart(t *testing.T) {
	_, ledger, guards := spendDB(t)
	ctx := context.Background()
	const sid = "incident-granted"

	first := restartPool(t, ledger, guards, nil)
	first.restore(ctx, sid)
	if err := first.meter(sid).Observe(spend24k()); !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("the fixture did not trip the ceiling: %v", err)
	}

	wds := newWatchdogPool(watchdog.ModeWarn)
	wds.durable(guards, discardLogger())
	view := &guardrailView{meters: first, wds: wds, logger: discardLogger()}
	if _, err := view.reset(sid, attach.GuardrailResetRequest{
		Guardrail:           attach.GuardrailCostCeiling,
		Caller:              "sre@example.com",
		AdditionalBudgetUSD: 1.00,
	}); err != nil {
		t.Fatalf("reset with a $1.00 grant: %v", err)
	}

	second := restartPool(t, ledger, guards, nil)
	second.restore(ctx, sid)
	if got := second.meter(sid).SessionLimits().MaxCostUSD; !sameUSD(got, 2.00) {
		t.Fatalf("cap after restart = $%.2f, want $2.00 (the bundle's $1.00 plus the operator's $1.00)", got)
	}
	if err := second.preflight(sid); err != nil {
		t.Fatalf("a session the operator rescued was wedged by the restart: %v", err)
	}
	// The grant is runway, not amnesty: the spend is still on the books.
	if _, cost, _ := second.meter(sid).Snapshot(); !sameUSD(cost, 1.20) {
		t.Errorf("restored spend = $%.4f, want the $1.2000 it actually spent", cost)
	}
}

// The wedge W10.2 opened and closed in the same change.
//
// preflight used to ask Trips() — has a ceiling been *crossed*. The
// pre-call gate means a well-behaved session now stops exactly *on* its
// cap without crossing anything, so Trips() is empty and the door swings
// open, and the session starts a turn whose every model call is refused
// and which answers "I am out of budget" in prose. Forever, on every
// schedule fire, looking like a session that is answering.
//
// 20k tokens at echo's $0.05/1K is $1.00 against a $1.00 cap: met, not
// crossed. Ask Trips() here and this test fails with a turn that ran.
func TestASessionStoppedExactlyOnItsCapIsRefusedAtTheDoor(t *testing.T) {
	_, ledger, guards := spendDB(t)
	ctx := context.Background()
	const sid = "incident-at-cap"

	first := restartPool(t, ledger, guards, nil)
	first.restore(ctx, sid)
	if err := first.meter(sid).Observe(spend("coordinator", 20_000)); err != nil {
		t.Fatalf("$1.00 against a $1.00 cap was reported as crossed (%v); this fixture is about the case where it is not", err)
	}
	if got := first.meter(sid).Trips(); len(got) != 0 {
		t.Fatalf("the post-hoc fold reports %d trips at exactly the cap; the wedge this test is about cannot happen", len(got))
	}

	next := &loopingModel{rounds: 2}
	h := newTurnHarnessOpts(t, next, watchdog.ModeWarn, pokeTool(t))
	h.meters = restartPool(t, ledger, guards, nil)

	err := h.turn(ctx, sid)
	if err == nil {
		t.Fatal("a session sitting exactly on its cost ceiling started a turn")
	}
	if !errors.Is(err, inject.ErrConflict) {
		t.Errorf("refusal err = %v, want inject.ErrConflict (409, the operator has to reset it)", err)
	}
	if got := next.calls.Load(); got != 0 {
		t.Errorf("the refused turn still called the model %d times, want 0", got)
	}
	if !strings.Contains(err.Error(), "guardrails/reset") {
		t.Errorf("refusal = %q, does not tell the operator how to clear it", err)
	}
}

// Per-specialist ceilings are the same defect with a different key, and
// the fix has to carry the author across the restart to re-attribute the
// spend against whatever scopes the current roster declares.
func TestScopedSpendSurvivesARestart(t *testing.T) {
	_, ledger, guards := spendDB(t)
	ctx := context.Background()
	const sid = "incident-scoped"

	// One specialist with a $0.25 ceiling of its own, under the bundle's
	// $1.00 — the shape internal/compose.MeterScopes produces.
	specs := []specialists.Spec{{Name: "OOMKilled", Budget: specialists.Budget{MaxCostUSD: 0.25}}}

	first := restartPool(t, ledger, guards, specs)
	first.restore(ctx, sid)
	// 4k tokens is $0.20: under the specialist's $0.25 and the session's.
	if err := first.meter(sid).Observe(spend("OOMKilled", 4_000)); err != nil {
		t.Fatalf("first scoped spend was refused: %v", err)
	}

	second := restartPool(t, ledger, guards, specs)
	second.restore(ctx, sid)
	_, cost, _, ok := second.meter(sid).ScopeSnapshot("OOMKilled")
	if !ok {
		t.Fatal("the restored meter carries no scope for OOMKilled")
	}
	if !sameUSD(cost, 0.20) {
		t.Fatalf("restored scope spend = $%.4f, want $0.2000", cost)
	}
	// $0.20 + $0.20 is over the specialist's $0.25 and under the
	// session's $1.00, so only the scope's ceiling can stop this.
	err := second.meter(sid).Observe(spend("OOMKilled", 4_000))
	if !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("the restored specialist ceiling did not bite: %v", err)
	}
	if !strings.Contains(err.Error(), "OOMKilled") {
		t.Errorf("error = %q, does not name the specialist whose ceiling stopped the run", err)
	}
}

// Fails open, loudly, for the reason the watchdog's restore does: a
// storage fault must not become an outage. The ceiling stays armed
// against this process's own spend, and the read is retried rather than
// remembered as "restored to nothing".
func TestBudgetRestoreFailsOpen(t *testing.T) {
	db, ledger, guards := spendDB(t)
	ctx := context.Background()
	const sid = "incident-unreadable"

	first := restartPool(t, ledger, guards, nil)
	first.restore(ctx, sid)
	if err := first.meter(sid).Observe(spend12k()); err != nil {
		t.Fatalf("first spend: %v", err)
	}
	if err := db.Exec("DROP TABLE agent_budget_spend").Error; err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}

	second := restartPool(t, ledger, guards, nil)
	second.restore(ctx, sid)
	if err := second.preflight(sid); err != nil {
		t.Fatalf("an unreadable ledger refused a turn: %v", err)
	}
	if second.restored[sid] {
		t.Error("a failed restore latched as done; the next turn will not retry")
	}
}

// Without a durable store — no --attach-listen, hence no reset endpoint
// either — the pool behaves exactly as it did before this existed.
func TestMeterPoolWithoutALedgerIsInert(t *testing.T) {
	ctx := context.Background()
	mp := newMeterPool(cappedBundle(), nil, "", "echo")

	mp.restore(ctx, "s-no-store")
	if err := mp.meter("s-no-store").Observe(spend12k()); err != nil {
		t.Fatalf("spend with no ledger: %v", err)
	}
	if mp.restored["s-no-store"] {
		t.Error("a pool with no store latched a session as restored")
	}
	// A second pool starts clean, which is the pre-#175 behavior and the
	// reason serve warns about it at startup.
	other := newMeterPool(cappedBundle(), nil, "", "echo")
	other.restore(ctx, "s-no-store")
	if _, cost, _ := other.meter("s-no-store").Snapshot(); cost != 0 {
		t.Errorf("a pool with no store restored $%.4f from somewhere", cost)
	}
}

// Restoring twice would double the spend, and the sessions this matters
// for are the ones already near a ceiling — where the double count is
// the difference between running and refused. The pool's latch is the
// first guard and the meter's is the one that holds under a race.
func TestRestoreIsIdempotent(t *testing.T) {
	_, ledger, guards := spendDB(t)
	ctx := context.Background()
	const sid = "incident-twice"

	first := restartPool(t, ledger, guards, nil)
	first.restore(ctx, sid)
	if err := first.meter(sid).Observe(spend12k()); err != nil {
		t.Fatalf("first spend: %v", err)
	}

	second := restartPool(t, ledger, guards, nil)
	for i := 0; i < 3; i++ {
		second.restore(ctx, sid)
	}
	if _, cost, _ := second.meter(sid).Snapshot(); !sameUSD(cost, 0.60) {
		t.Fatalf("three restores produced $%.4f, want $0.6000 once", cost)
	}
	if err := second.meter(sid).Restore(budget.Prior{Session: budget.Totals{CostUSD: 5}}); !errors.Is(err, budget.ErrRestored) {
		t.Fatalf("a direct second Restore returned %v, want budget.ErrRestored", err)
	}
}

// sameUSD compares dollars the way the meter accumulates them: by
// summing floats. 12 * 0.05 is 0.6000000000000001, and a test that
// insists otherwise is testing IEEE 754, not the ledger.
func sameUSD(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// 12k tokens at echo's $0.05/1K is $0.60; 24k is $1.20, over the
// fixture's $1.00 cap in a single call.
func spend12k() *adksession.Event { return spend("coordinator", 12_000) }
func spend24k() *adksession.Event { return spend("coordinator", 24_000) }
