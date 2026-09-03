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

// #274. budget_durable_test.go proves the ledger works; it opens its own
// gorm connection with spendDB, so it never touches the wiring that
// decides whether a daemon gets one. That gap is the bug: the stores hung
// off the eventlog Handle, the Handle is built only under
// --attach-listen, and so a daemon with --session-db and no attach socket
// had durable sessions and a budget ceiling that reset every restart.
//
// These tests go through buildSessionService — the seam that was
// broken — rather than around it.

package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/eventlog"
	"github.com/go-steer/mast/pkg/watchdog"
)

// noAttachStores is one "process" on the plain path: open the session
// backend the way serve() does without --attach-listen, then put the two
// durable stores on the connection it hands back.
func noAttachStores(t *testing.T, dsn string) (*eventlog.SpendStore, *eventlog.GuardrailStore) {
	t.Helper()
	ctx := context.Background()
	_, db, err := buildSessionService(ctx, "sqlite", dsn, discardLogger())
	if err != nil {
		t.Fatalf("buildSessionService(sqlite, %q): %v", dsn, err)
	}
	if db == nil {
		t.Fatal("no DB from the plain session path, so no ledger can be wired — this is the #274 regression")
	}
	ledger, err := eventlog.NewSpendStore(ctx, db)
	if err != nil {
		t.Fatalf("NewSpendStore on the plain path: %v", err)
	}
	guards, err := eventlog.NewGuardrailStore(ctx, db)
	if err != nil {
		t.Fatalf("NewGuardrailStore on the plain path: %v", err)
	}
	return ledger, guards
}

// The headline, restated for the deployment that actually needs it: an
// unattended daemon with no operator socket. A crash loop must not be
// able to spend the cap once per restart.
func TestBudgetSpendSurvivesARestartWithoutAnAttachListener(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "sessions.db")
	ctx := context.Background()
	const sid = "incident-unattended"

	// Process one: $0.60 of a $1.00 cap.
	ledger, guards := noAttachStores(t, dsn)
	first := restartPool(t, ledger, guards, nil)
	first.restore(ctx, sid)
	if err := first.meter(sid).Observe(spend12k()); err != nil {
		t.Fatalf("first spend was refused under a $1.00 cap: %v", err)
	}
	if _, cost, _ := first.meter(sid).Snapshot(); !sameUSD(cost, 0.60) {
		t.Fatalf("first process spent $%.4f, want $0.6000; the fixture is wrong", cost)
	}

	// Process two: the same database file, opened from scratch the same
	// way, with no memory of the first.
	ledger2, guards2 := noAttachStores(t, dsn)
	second := restartPool(t, ledger2, guards2, nil)
	if _, cost, _ := second.meter(sid).Snapshot(); cost != 0 {
		t.Fatalf("the fresh pool started at $%.4f before it read anything", cost)
	}

	second.restore(ctx, sid)
	tokens, cost, calls := second.meter(sid).Snapshot()
	if !sameUSD(cost, 0.60) || tokens != 12_000 || calls != 1 {
		t.Fatalf("after restart: $%.4f over %d calls (%d tokens), want $0.6000 over 1 call (12000 tokens)", cost, calls, tokens)
	}

	// And the ceiling bites where it would have without the restart.
	if err := second.meter(sid).Observe(spend12k()); !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("the restored meter allowed $1.20 against a $1.00 cap: %v", err)
	}
}

// A grant is issued through the attach surface, but it must not stop
// meaning something on a later run that has no attach surface. Restoring
// spend without replaying the grant that answered it would wedge a
// session an operator already rescued — by a restart they never made, and
// with their own grant sitting in the audit log they can no longer reach.
//
// This is why meterPool.durable still takes the guardrail store on the
// plain path, even though nothing there can write a new grant.
func TestAnOperatorGrantStillCountsOnADaemonWithNoAttachSurface(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "sessions.db")
	ctx := context.Background()
	const sid = "incident-rescued"

	ledger, guards := noAttachStores(t, dsn)
	first := restartPool(t, ledger, guards, nil)
	first.restore(ctx, sid)
	// $1.20 against a $1.00 cap: over, in one call.
	if err := first.meter(sid).Observe(spend24k()); !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("the fixture did not trip the ceiling: %v", err)
	}

	// The rescue, recorded the way the attach endpoint records it: a
	// $1.00 grant on top of the bundle's $1.00 cap.
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

	// Restart with no attach listener at all.
	ledger2, guards2 := noAttachStores(t, dsn)
	second := restartPool(t, ledger2, guards2, nil)
	second.restore(ctx, sid)

	if got := second.meter(sid).SessionLimits().MaxCostUSD; !sameUSD(got, 2.00) {
		t.Fatalf("cap after restart = $%.2f, want $2.00 (the bundle's $1.00 plus the operator's $1.00)", got)
	}
	if _, cost, _ := second.meter(sid).Snapshot(); !sameUSD(cost, 1.20) {
		t.Fatalf("restored spend $%.4f, want $1.2000", cost)
	}
	// $1.20 spent against a granted $2.00 leaves room for another $0.60.
	if err := second.meter(sid).Observe(spend12k()); err != nil {
		t.Fatalf("a session the operator rescued to $2.00 was refused at $1.80: %v", err)
	}
}
