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

// A halt that a restart clears is not a halt. These tests simulate the
// restart the way it actually happens — a fresh watchdogPool over the
// same database — and assert the enforce-mode backstop is still armed
// on the other side of it.
package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/eventlog"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/watchdog"
)

// openGuardrailDB returns an on-disk SQLite connection plus a store
// over it. On-disk rather than ":memory:" because the whole point is
// that a second connection — standing in for the next process — reads
// what the first one wrote.
func openGuardrailDB(t *testing.T) (*gorm.DB, *eventlog.GuardrailStore) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "guardrails.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	store, err := eventlog.NewGuardrailStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewGuardrailStore: %v", err)
	}
	return db, store
}

// durableHarness is a turn harness whose watchdog pool persists trips —
// one "process". Restarting means building another one over the same
// store.
//
// Two fixture sizes below: rounds=60 is a runaway loop, well past
// RepeatedToolCallSignal's threshold of 5, and rounds=2 is a clean turn
// that could not trip anything — so a turn that fails on a rounds=2
// harness failed because of restored state, not because it looped.
func durableHarness(t *testing.T, m *loopingModel, mode watchdog.Mode, store *eventlog.GuardrailStore) *turnHarness {
	t.Helper()
	h := newTurnHarnessOpts(t, m, mode, pokeTool(t))
	h.wds.durable(store, discardLogger())
	return h
}

// The headline. A daemon that crashes mid-loop restarts automatically
// and unattended; without the durable trip the loop → halt → crash →
// restart cycle enforce mode exists to break simply resumes, each
// restart handing the loop a clean backstop.
func TestWatchdogHaltSurvivesARestart(t *testing.T) {
	_, store := openGuardrailDB(t)
	ctx := context.Background()
	const sid = "s-durable"

	first := durableHarness(t, &loopingModel{rounds: 60}, watchdog.ModeEnforce, store)
	if err := first.turn(ctx, sid); !watchdog.IsTripped(err) {
		t.Fatalf("first turn err = %v, want a watchdog halt", err)
	}

	// The restart: a brand-new pool with no memory of the trip, reading
	// the same database.
	next := &loopingModel{rounds: 60}
	second := durableHarness(t, next, watchdog.ModeEnforce, store)
	if halted, _ := second.wds.halted(sid); halted {
		t.Fatal("the fresh pool started out halted before it read anything")
	}

	err := second.turn(ctx, sid)
	if !errors.Is(err, inject.ErrConflict) || !watchdog.IsTripped(err) {
		t.Fatalf("turn after restart: err = %v, want a watchdog halt with ErrConflict", err)
	}
	// Refused before the model, not after: a restored halt that still
	// pays for a round-trip has not restored anything worth having.
	if got := next.calls.Load(); got != 0 {
		t.Errorf("the restored halt still called the model %d times", got)
	}
	// The operator reads the sentence the original halt was written
	// with, including the way out.
	if !strings.Contains(err.Error(), "guardrails/reset") {
		t.Errorf("restored halt = %q, does not tell the operator how to clear it", err)
	}
	if !strings.Contains(err.Error(), "repeated-tool-call") {
		t.Errorf("restored halt = %q, does not name the signal that fired", err)
	}
}

// And the way out still works across the restart: the reset clears the
// halt in this process AND durably, so a third process does not
// re-restore a halt the operator already cleared.
func TestWatchdogResetClearsTheHaltDurably(t *testing.T) {
	_, store := openGuardrailDB(t)
	ctx := context.Background()
	const sid = "s-durable-reset"

	first := durableHarness(t, &loopingModel{rounds: 60}, watchdog.ModeEnforce, store)
	if err := first.turn(ctx, sid); !watchdog.IsTripped(err) {
		t.Fatalf("first turn err = %v, want a watchdog halt", err)
	}

	second := durableHarness(t, &loopingModel{rounds: 2}, watchdog.ModeEnforce, store)
	view := &guardrailView{meters: second.meters, wds: second.wds, logger: discardLogger()}

	// The projection must show the restored halt: an operator polling a
	// freshly restarted daemon cannot be told the session is healthy
	// right up until the next turn refuses.
	if got := view.info(sid); !got.Watchdog.Tripped || !got.Halted {
		t.Fatalf("guardrails after restart = %+v, want the restored halt visible", got.Watchdog)
	}

	resp, err := view.reset(sid, attach.GuardrailResetRequest{
		Guardrail: attach.GuardrailWatchdog, Caller: "sre@example.com",
	})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(resp.Reset) != 1 || resp.Reset[0] != attach.GuardrailWatchdog {
		t.Fatalf("reset = %v, want the restored watchdog halt cleared", resp.Reset)
	}
	if err := second.turn(ctx, sid); err != nil {
		t.Fatalf("turn after reset: %v", err)
	}

	// A third process must not restore what the operator cleared.
	third := durableHarness(t, &loopingModel{rounds: 2}, watchdog.ModeEnforce, store)
	if err := third.turn(ctx, sid); err != nil {
		t.Fatalf("turn after a second restart: %v", err)
	}
}

// Configuration still wins over history. A deployment dialed back to
// feedback must not inherit a halt only enforce could have produced —
// nothing below enforce halts, so nothing below enforce should adopt
// one either.
func TestWatchdogHaltIsNotRestoredBelowEnforce(t *testing.T) {
	_, store := openGuardrailDB(t)
	ctx := context.Background()
	const sid = "s-dialed-back"

	first := durableHarness(t, &loopingModel{rounds: 60}, watchdog.ModeEnforce, store)
	if err := first.turn(ctx, sid); !watchdog.IsTripped(err) {
		t.Fatalf("first turn err = %v, want a watchdog halt", err)
	}

	for _, mode := range []watchdog.Mode{watchdog.ModeWarn, watchdog.ModeFeedback} {
		t.Run(string(mode), func(t *testing.T) {
			h := durableHarness(t, &loopingModel{rounds: 2}, mode, store)
			if err := h.turn(ctx, sid); err != nil {
				t.Fatalf("%s inherited an enforce-mode halt: %v", mode, err)
			}
			if halted, _ := h.wds.halted(sid); halted {
				t.Errorf("%s reported a halt it could never have produced", mode)
			}
		})
	}
}

// Fails open, loudly. A guardrail that cannot be read is a reason to
// log and continue: a corrupt or locked-out table would otherwise halt
// every session in the deployment with no trip behind it, converting a
// storage fault into an outage.
func TestWatchdogRestoreFailsOpen(t *testing.T) {
	db, store := openGuardrailDB(t)
	ctx := context.Background()
	const sid = "s-unreadable"

	first := durableHarness(t, &loopingModel{rounds: 60}, watchdog.ModeEnforce, store)
	if err := first.turn(ctx, sid); !watchdog.IsTripped(err) {
		t.Fatalf("first turn err = %v, want a watchdog halt", err)
	}
	if err := db.Exec("DROP TABLE agent_guardrail_log").Error; err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}

	second := durableHarness(t, &loopingModel{rounds: 2}, watchdog.ModeEnforce, store)
	if err := second.turn(ctx, sid); err != nil {
		t.Fatalf("an unreadable guardrail table refused a turn: %v", err)
	}
	// The read failure must not latch: the next turn tries again, so a
	// transient fault costs one unguarded turn rather than the rest of
	// the process's life.
	if second.wds.restored[sid] {
		t.Error("a failed restore latched as done; the next turn will not retry")
	}
}

// The reset row is the audit trail the log line alone could not be: it
// survives the process, and it says who.
func TestGuardrailResetIsRecordedDurably(t *testing.T) {
	_, store := openGuardrailDB(t)
	ctx := context.Background()
	const sid = "s-audit"

	wds := newWatchdogPool(watchdog.ModeEnforce)
	wds.durable(store, discardLogger())
	view := &guardrailView{
		meters: &meterPool{
			cfg:  budget.Config{Limits: budget.Limits{MaxCostUSD: 1, RatePer1K: 0.05}},
			byID: map[string]*budget.Meter{},
		},
		wds:    wds,
		logger: discardLogger(),
	}

	if _, err := view.reset(sid, attach.GuardrailResetRequest{
		Guardrail:           attach.GuardrailCostCeiling,
		Caller:              "sre@example.com",
		AdditionalBudgetUSD: 2.50,
	}); err != nil {
		t.Fatalf("reset: %v", err)
	}

	rows, err := store.History(ctx, appName, defaultUserID, sid)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("History = %d rows, want the reset recorded", len(rows))
	}
	got := rows[0]
	if got.Kind != eventlog.GuardrailKindReset || got.Guardrail != attach.GuardrailCostCeiling {
		t.Errorf("row = %+v, want a cost_ceiling reset", got)
	}
	if got.Caller != "sre@example.com" {
		t.Errorf("caller = %q, the audit trail cannot say who", got.Caller)
	}
	if got.BudgetAddedUSD != 2.50 {
		t.Errorf("budget added = %v, want 2.50", got.BudgetAddedUSD)
	}
}

// A refused reset must leave nothing behind, in the log as in memory:
// an audit row for an intervention that did not happen is worse than
// no row.
func TestRefusedResetIsNotRecorded(t *testing.T) {
	_, store := openGuardrailDB(t)
	ctx := context.Background()
	const sid = "s-refused"

	wds := newWatchdogPool(watchdog.ModeEnforce)
	wds.durable(store, discardLogger())
	view := &guardrailView{
		meters: &meterPool{
			cfg:  budget.Config{Limits: budget.Limits{MaxCostUSD: 0.0001, RatePer1K: 5}},
			byID: map[string]*budget.Meter{},
		},
		wds:    wds,
		logger: discardLogger(),
	}
	m := view.meters.meter(sid)
	_ = m.Observe(spend("coordinator", 400))
	if len(m.Trips()) == 0 {
		t.Fatal("the meter did not trip; the fixture is wrong")
	}

	// A bare reset on a tripped ceiling re-trips, so it is refused
	// before anything is mutated.
	if _, err := view.reset(sid, attach.GuardrailResetRequest{Caller: "sre@example.com"}); err == nil {
		t.Fatal("a re-tripping reset was accepted")
	}
	rows, err := store.History(ctx, appName, defaultUserID, sid)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a refused reset wrote %d audit rows: %+v", len(rows), rows)
	}
}

// Without a durable store — no --attach-listen, hence no reset endpoint
// either — the pool behaves exactly as it did before this existed.
func TestWatchdogPoolWithoutAStoreIsInert(t *testing.T) {
	ctx := context.Background()
	h := newTurnHarnessOpts(t, &loopingModel{rounds: 60}, watchdog.ModeEnforce, pokeTool(t))
	if err := h.turn(ctx, "s-no-store"); !watchdog.IsTripped(err) {
		t.Fatalf("turn err = %v, want a watchdog halt", err)
	}
	// restore, recordTrip and recordReset all ran with a nil store and
	// none of them panicked or refused anything.
	h.wds.restore(ctx, "s-no-store")
	if halted, _ := h.wds.halted("s-no-store"); !halted {
		t.Error("restore with no store cleared a live halt")
	}
	h.wds.recordReset("s-no-store", attach.GuardrailAll, "test", attach.GuardrailResetResponse{})
}
