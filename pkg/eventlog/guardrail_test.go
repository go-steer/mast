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

package eventlog

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

const (
	gApp  = "mast"
	gUser = "operator"
	gSID  = "incident-1"
)

// openGuardrailStore returns a store over a fresh on-disk SQLite
// database, plus the raw handle so a test can reopen it and prove the
// state survived the process that wrote it.
func openGuardrailStore(t *testing.T) (*GuardrailStore, *gorm.DB) {
	t.Helper()
	h, err := Open(context.Background(), sqlite.Open(filepath.Join(t.TempDir(), "eventlog.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	s, err := NewGuardrailStore(context.Background(), h.DB)
	if err != nil {
		t.Fatalf("NewGuardrailStore: %v", err)
	}
	return s, h.DB
}

func TestGuardrailStoreFoldsTripAndReset(t *testing.T) {
	ctx := context.Background()
	s, _ := openGuardrailStore(t)

	// Nothing recorded: the overwhelmingly common case must be a clean
	// zero value, not an error a caller has to special-case.
	st, err := s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold on an empty log: %v", err)
	}
	if st.Halted() {
		t.Fatalf("a session with no guardrail rows folded to halted: %+v", st)
	}

	trippedAt := time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)
	err = s.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
		Kind:      GuardrailKindTrip,
		Guardrail: GuardrailWatchdog,
		Signal:    "repeated_tool_call",
		Reason:    "watchdog halted this session (repeated_tool_call): kubectl get pods x6",
		At:        trippedAt,
	})
	if err != nil {
		t.Fatalf("Append trip: %v", err)
	}

	st, err = s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold after trip: %v", err)
	}
	if !st.WatchdogTripped || !st.Halted() {
		t.Fatalf("trip did not latch: %+v", st)
	}
	if st.WatchdogSignal != "repeated_tool_call" {
		t.Errorf("signal = %q, want repeated_tool_call", st.WatchdogSignal)
	}
	// Carried verbatim, not reconstructed: an operator reading a
	// restored halt is reading the sentence the halt was written with.
	if !strings.Contains(st.WatchdogReason, "kubectl get pods x6") {
		t.Errorf("reason = %q, want the original halt text", st.WatchdogReason)
	}
	if !st.TrippedAt.Equal(trippedAt) {
		t.Errorf("TrippedAt = %v, want %v", st.TrippedAt, trippedAt)
	}

	if err := s.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
		Kind:      GuardrailKindReset,
		Guardrail: GuardrailWatchdog,
		Caller:    "sre@example.com",
		Reason:    "cleared watchdog",
	}); err != nil {
		t.Fatalf("Append reset: %v", err)
	}
	st, err = s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold after reset: %v", err)
	}
	if st.Halted() {
		t.Fatalf("reset did not clear the halt: %+v", st)
	}
	if st.WatchdogReason != "" || !st.TrippedAt.IsZero() {
		t.Errorf("reset left halt residue behind: %+v", st)
	}
}

// A trip is a latch, not a tally: tripping twice and resetting once
// leaves the session clear. The opposite (a counter) would need as many
// resets as trips, which no operator could know the count of.
func TestGuardrailStoreTripIsALatchNotATally(t *testing.T) {
	ctx := context.Background()
	s, _ := openGuardrailStore(t)
	for i := 0; i < 3; i++ {
		if err := s.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
			Kind: GuardrailKindTrip, Guardrail: GuardrailWatchdog,
			Signal: "tool_failure_streak", Reason: "5 consecutive failures",
		}); err != nil {
			t.Fatalf("Append trip %d: %v", i, err)
		}
	}
	if err := s.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
		Kind: GuardrailKindReset, Guardrail: GuardrailAll,
	}); err != nil {
		t.Fatalf("Append reset: %v", err)
	}
	st, err := s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Halted() {
		t.Fatalf("one reset did not clear three trips: %+v", st)
	}
}

// The mirror image of the latch: granted runway accumulates, because
// two resets that each hand over $5 have handed over $10.
func TestGuardrailStoreGrantsAccumulate(t *testing.T) {
	ctx := context.Background()
	s, _ := openGuardrailStore(t)
	for i := 0; i < 2; i++ {
		if err := s.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
			Kind: GuardrailKindReset, Guardrail: GuardrailCostCeiling,
			BudgetAddedUSD: 5, TokensAdded: 1000, TurnsAdded: 3,
		}); err != nil {
			t.Fatalf("Append reset %d: %v", i, err)
		}
	}
	st, err := s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.BudgetAddedUSD != 10 || st.TokensAdded != 2000 || st.TurnsAdded != 6 {
		t.Fatalf("grants did not accumulate: %+v", st)
	}
}

// GuardrailAll clears both latches; a targeted reset clears only its
// own. An operator clearing the budget must not silently disarm the
// watchdog halt that is the reason the session is stuck.
func TestGuardrailStoreResetTargetsOneGuardrail(t *testing.T) {
	ctx := context.Background()
	s, _ := openGuardrailStore(t)
	appendAll := func() {
		t.Helper()
		for _, g := range []string{GuardrailWatchdog, GuardrailCostCeiling} {
			if err := s.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
				Kind: GuardrailKindTrip, Guardrail: g, Reason: g + " tripped",
			}); err != nil {
				t.Fatalf("Append trip %s: %v", g, err)
			}
		}
	}
	appendAll()
	if err := s.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
		Kind: GuardrailKindReset, Guardrail: GuardrailCostCeiling, BudgetAddedUSD: 2,
	}); err != nil {
		t.Fatalf("Append targeted reset: %v", err)
	}
	st, err := s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.CostTripped {
		t.Errorf("targeted reset left the cost ceiling tripped: %+v", st)
	}
	if !st.WatchdogTripped {
		t.Errorf("a cost-ceiling reset cleared the watchdog halt too: %+v", st)
	}

	if err := s.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
		Kind: GuardrailKindReset, Guardrail: GuardrailAll,
	}); err != nil {
		t.Fatalf("Append reset all: %v", err)
	}
	if st, err = s.Fold(ctx, gApp, gUser, gSID); err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Halted() {
		t.Fatalf("reset all left something tripped: %+v", st)
	}
}

// Rows are keyed by the (app, user, session) triple every other
// mast-owned table keys on: one session's halt must not refuse another
// session's turns.
func TestGuardrailStoreIsolatesSessions(t *testing.T) {
	ctx := context.Background()
	s, _ := openGuardrailStore(t)
	if err := s.Append(ctx, gApp, gUser, "incident-a", GuardrailRecord{
		Kind: GuardrailKindTrip, Guardrail: GuardrailWatchdog, Reason: "looping",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	st, err := s.Fold(ctx, gApp, gUser, "incident-b")
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Halted() {
		t.Fatalf("incident-a's halt leaked into incident-b: %+v", st)
	}
	if st, err = s.Fold(ctx, gApp, "someone-else", "incident-a"); err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Halted() {
		t.Fatalf("one user's halt leaked to another: %+v", st)
	}
}

// The point of the whole file: a halt written by one process is read by
// the next one. A second store over the same file stands in for the
// restart.
func TestGuardrailStoreSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "eventlog.db")

	h1, err := Open(ctx, sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s1, err := NewGuardrailStore(ctx, h1.DB)
	if err != nil {
		t.Fatalf("NewGuardrailStore: %v", err)
	}
	if err := s1.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
		Kind: GuardrailKindTrip, Guardrail: GuardrailWatchdog,
		Signal: "repeated_tool_call", Reason: "halted: kubectl get pods x6",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := h1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	h2, err := Open(ctx, sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = h2.Close() }()
	// AutoMigrate a second time over an existing table: a restart must
	// not need a fresh database.
	s2, err := NewGuardrailStore(ctx, h2.DB)
	if err != nil {
		t.Fatalf("NewGuardrailStore on an existing db: %v", err)
	}
	st, err := s2.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if !st.WatchdogTripped {
		t.Fatalf("the halt did not survive the reopen: %+v", st)
	}
	if st.WatchdogSignal != "repeated_tool_call" {
		t.Errorf("signal = %q after reopen", st.WatchdogSignal)
	}
}

// History is the audit trail: every intervention, oldest first, with
// who did it. The fold deliberately forgets cleared trips; History must
// not.
func TestGuardrailStoreHistoryKeepsClearedTrips(t *testing.T) {
	ctx := context.Background()
	s, _ := openGuardrailStore(t)
	rows := []GuardrailRecord{
		{Kind: GuardrailKindTrip, Guardrail: GuardrailWatchdog, Signal: "repeated_tool_call"},
		{Kind: GuardrailKindReset, Guardrail: GuardrailAll, Caller: "sre@example.com"},
		{Kind: GuardrailKindTrip, Guardrail: GuardrailWatchdog, Signal: "cycle"},
	}
	for i, r := range rows {
		if err := s.Append(ctx, gApp, gUser, gSID, r); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	got, err := s.History(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("History returned %d rows, want 3", len(got))
	}
	if got[0].Signal != "repeated_tool_call" || got[2].Signal != "cycle" {
		t.Errorf("History is not oldest-first: %+v", got)
	}
	if got[1].Caller != "sre@example.com" {
		t.Errorf("History lost the caller: %+v", got[1])
	}
	if got[0].At.IsZero() {
		t.Errorf("a zero At was not stamped at append time: %+v", got[0])
	}
}

// A nil store is the "no durable store configured" wiring, and every
// call site relies on it being usable rather than guarded.
func TestGuardrailStoreNilIsInert(t *testing.T) {
	ctx := context.Background()
	var s *GuardrailStore
	if err := s.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
		Kind: GuardrailKindTrip, Guardrail: GuardrailWatchdog,
	}); err != nil {
		t.Fatalf("Append on a nil store: %v", err)
	}
	st, err := s.Fold(ctx, gApp, gUser, gSID)
	if err != nil {
		t.Fatalf("Fold on a nil store: %v", err)
	}
	if st.Halted() {
		t.Fatalf("a nil store reported a halt: %+v", st)
	}
	h, err := s.History(ctx, gApp, gUser, gSID)
	if err != nil || len(h) != 0 {
		t.Fatalf("History on a nil store: %v / %d rows", err, len(h))
	}
}

// A malformed row is refused at the boundary rather than written and
// then silently skipped by the fold, where nobody would ever see it.
func TestGuardrailStoreRejectsMalformedRecords(t *testing.T) {
	ctx := context.Background()
	s, _ := openGuardrailStore(t)
	cases := []struct {
		name string
		app  string
		sid  string
		rec  GuardrailRecord
		want string
	}{
		{"unknown kind", gApp, gSID, GuardrailRecord{Kind: "halt", Guardrail: GuardrailWatchdog}, "unknown kind"},
		{"empty kind", gApp, gSID, GuardrailRecord{Guardrail: GuardrailWatchdog}, "unknown kind"},
		{"no guardrail", gApp, gSID, GuardrailRecord{Kind: GuardrailKindTrip}, "guardrail is required"},
		{"no session", gApp, "", GuardrailRecord{Kind: GuardrailKindTrip, Guardrail: GuardrailWatchdog}, "session are required"},
		{"no app", "", gSID, GuardrailRecord{Kind: GuardrailKindTrip, Guardrail: GuardrailWatchdog}, "app and session"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.Append(ctx, c.app, gUser, c.sid, c.rec)
			if err == nil {
				t.Fatalf("Append accepted %+v", c.rec)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
	if got, _ := s.History(ctx, gApp, gUser, gSID); len(got) != 0 {
		t.Fatalf("a refused Append still wrote %d rows", len(got))
	}
}

// The trip path runs from a turn's alert tap while an attach handler
// can be reading or resetting the same session.
func TestGuardrailStoreConcurrentAppendAndFold(t *testing.T) {
	ctx := context.Background()
	s, _ := openGuardrailStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := s.Append(ctx, gApp, gUser, gSID, GuardrailRecord{
				Kind: GuardrailKindTrip, Guardrail: GuardrailWatchdog, Signal: "repeated_tool_call",
			}); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.Fold(ctx, gApp, gUser, gSID); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent access: %v", err)
	}
}

func TestNewGuardrailStoreRequiresDB(t *testing.T) {
	if _, err := NewGuardrailStore(context.Background(), nil); err == nil {
		t.Fatal("NewGuardrailStore(nil) returned no error")
	}
}
