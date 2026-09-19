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

// #345: a second replica of a scheduled workload fires everything
// twice and says nothing about it. These tests are about the decision
// and the log line, because for an operator the log line *is* the
// feature — the failure it replaces is one you would otherwise diagnose
// from duplicate side effects at the far end.
package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/go-steer/mast/pkg/eventlog"
)

func schedLeaseDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "sessions.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	return db
}

func capturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// acquireNow is the contested path without the real boot wait. The wait
// itself is covered by the retry-window tests below; everywhere else it
// would only buy ten seconds per assertion.
func acquireNow(ctx context.Context, db *gorm.DB, workloadName string, logger *slog.Logger) *schedulingLease {
	return acquireSchedulingLeaseWithin(ctx, db, workloadName, logger, 0, time.Millisecond)
}

// The headline. Two daemons, one session store: one drives the
// unrequested work and the other does not, which is the difference
// between an operator scaling to 2 and getting one nightly run instead
// of two.
func TestAcquireSchedulingLease_SecondInstanceIsPassive(t *testing.T) {
	t.Parallel()
	db := schedLeaseDB(t)
	ctx := context.Background()

	firstLog, firstBuf := capturingLogger()
	first := acquireSchedulingLease(ctx, db, "nightly", firstLog)
	defer first.release()
	if !first.drivesScheduledWork() {
		t.Fatalf("the first instance must drive the work; log was %s", firstBuf.String())
	}

	secondLog, secondBuf := capturingLogger()
	second := acquireNow(ctx, db, "nightly", secondLog)
	defer second.release()
	if second.drivesScheduledWork() {
		t.Fatalf("the second instance must be passive; log was %s", secondBuf.String())
	}

	// The refusal has to be findable and has to say what stopped. An
	// operator reading it is trying to answer "why did my schedule not
	// fire on this pod", and a bare "lease held" does not answer it.
	got := secondBuf.String()
	if !strings.Contains(got, "level=ERROR") {
		t.Errorf("the refusal should be ERROR, not a line that scrolls past; got %s", got)
	}
	for _, want := range []string{"scheduled trigger", "timed-pause resume", "auto-resume"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not mention %q, so it does not say what stopped; got %s", want, got)
		}
	}
}

// Releasing hands the role over rather than making the successor wait
// out the staleness window — the rolling-restart case.
func TestAcquireSchedulingLease_ReleaseHandsTheRoleOver(t *testing.T) {
	t.Parallel()
	db := schedLeaseDB(t)
	ctx := context.Background()
	logger, _ := capturingLogger()

	first := acquireSchedulingLease(ctx, db, "nightly", logger)
	if !first.drivesScheduledWork() {
		t.Fatal("first instance should be active")
	}
	if err := first.release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	second := acquireSchedulingLease(ctx, db, "nightly", logger)
	defer second.release()
	if !second.drivesScheduledWork() {
		t.Error("after a clean release the successor should take the role immediately")
	}
}

// Two workloads over one store are not duplicating each other, and the
// lease must not pretend otherwise.
func TestAcquireSchedulingLease_DifferentWorkloadsBothDrive(t *testing.T) {
	t.Parallel()
	db := schedLeaseDB(t)
	ctx := context.Background()
	logger, _ := capturingLogger()

	a := acquireSchedulingLease(ctx, db, "nightly", logger)
	defer a.release()
	b := acquireSchedulingLease(ctx, db, "hourly", logger)
	defer b.release()
	if !a.drivesScheduledWork() || !b.drivesScheduledWork() {
		t.Errorf("different workloads should both drive their own schedules; nightly=%v hourly=%v",
			a.drivesScheduledWork(), b.drivesScheduledWork())
	}
}

// No durable store: nothing to coordinate through, so the daemon keeps
// working — but it says so, naming the flag, because this is the
// configuration where the trap is still armed.
func TestAcquireSchedulingLease_NoStoreStillDrivesAndWarns(t *testing.T) {
	t.Parallel()
	logger, buf := capturingLogger()

	lease := acquireSchedulingLease(context.Background(), nil, "nightly", logger)
	defer lease.release()
	if !lease.drivesScheduledWork() {
		t.Fatal("without a session store the daemon must still fire its own schedule")
	}
	got := buf.String()
	if !strings.Contains(got, "--session-db") {
		t.Errorf("the warning should name the flag that would fix it; got %s", got)
	}
}

// The deliberate fail-open. A store that cannot answer at boot must not
// silently turn a single-replica deployment into one whose schedule
// never fires again — the harm from guessing "active" needs a second
// replica to actually exist, and the harm from guessing "passive" does
// not.
func TestAcquireSchedulingLease_AStoreThatCannotAnswerFailsOpen(t *testing.T) {
	t.Parallel()
	db := schedLeaseDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close the pool out from under gorm: %v", err)
	}

	logger, buf := capturingLogger()
	lease := acquireSchedulingLease(context.Background(), db, "nightly", logger)
	defer lease.release()
	if !lease.drivesScheduledWork() {
		t.Fatalf("a broken store must not stop scheduled work; log was %s", buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "level=ERROR") {
		t.Errorf("failing open is a decision worth an ERROR, since it is the branch that can duplicate; got %s", got)
	}
	if !strings.Contains(got, "duplicates") {
		t.Errorf("the line should say what the fail-open risks; got %s", got)
	}
}

// plantAbandonedLease writes the lease row a SIGKILLed daemon leaves
// behind: a real row, a holder that is not heartbeating, and a last
// heartbeat staleFor in the past. It learns the key columns from a real
// acquisition rather than hardcoding eventlog's sentinels, so it cannot
// drift away from the thing it is imitating.
func plantAbandonedLease(t *testing.T, db *gorm.DB, workloadName string, staleFor time.Duration) {
	t.Helper()
	logger, _ := capturingLogger()
	real := acquireSchedulingLease(context.Background(), db, workloadName, logger)
	if !real.drivesScheduledWork() {
		t.Fatal("setup: could not take the lease to learn its key")
	}
	var key struct {
		AppName   string
		UserID    string
		SessionID string
	}
	if err := db.Table("agent_run_lock").Select("app_name", "user_id", "session_id").Take(&key).Error; err != nil {
		t.Fatalf("setup: read the planted key: %v", err)
	}
	if err := real.release(); err != nil {
		t.Fatalf("setup: release: %v", err)
	}
	now := time.Now().UTC()
	err := db.Exec(
		"INSERT INTO agent_run_lock (app_name, user_id, session_id, holder, acquired_at, heartbeat_at) VALUES (?, ?, ?, ?, ?, ?)",
		key.AppName, key.UserID, key.SessionID, "killed-instance/1/deadbeef", now.Add(-time.Hour), now.Add(-staleFor),
	).Error
	if err != nil {
		t.Fatalf("setup: plant the abandoned lease: %v", err)
	}
}

// The crash case, and the reason the retry window exists at all. A
// daemon that is SIGKILLed never releases its lease, so the next
// instance to boot finds the lease held — by a corpse, a moment ago.
// Refusing there would mean an OOM kill silently ends scheduled work
// until somebody restarts the pod a second time, which is strictly
// worse than the duplicate-fire bug the lease was added to fix.
//
// The planted holder is a foreign ID and the acquirer mints its own, so
// what this asserts is the real contract: staleness alone decides, and
// the taker need not be the process that died. The pod-restart case is
// one instance of that, not the rule.
func TestAcquireSchedulingLease_BootAfterAKillTakesTheAbandonedLease(t *testing.T) {
	t.Parallel()
	db := schedLeaseDB(t)
	// Fresh enough that a no-retry acquisition would refuse it, stale
	// within the window we are about to wait.
	plantAbandonedLease(t, db, "nightly", eventlog.InstanceLeaseStaleAfter-300*time.Millisecond)

	refusedLog, refusedBuf := capturingLogger()
	refused := acquireNow(context.Background(), db, "nightly", refusedLog)
	if refused.drivesScheduledWork() {
		t.Fatalf("setup is not testing anything: the planted lease was already stale; log was %s", refusedBuf.String())
	}

	logger, buf := capturingLogger()
	start := time.Now()
	lease := acquireSchedulingLeaseWithin(context.Background(), db, "nightly", logger,
		eventlog.InstanceLeaseStaleAfter+2*time.Second, 50*time.Millisecond)
	defer lease.release()

	if !lease.drivesScheduledWork() {
		t.Fatalf("an instance booting after a kill must take the abandoned lease, or the crash ends scheduled work for good; waited %s, log was %s",
			time.Since(start).Round(time.Millisecond), buf.String())
	}
	got := buf.String()
	// The pause at boot has to be explained, or it reads as a hang.
	if !strings.Contains(got, "waiting in case its holder is gone") {
		t.Errorf("the wait should say why boot paused; got %s", got)
	}
	if !strings.Contains(got, "stopped heartbeating") {
		t.Errorf("taking over should say it took over, not look like an ordinary boot; got %s", got)
	}
}

// The other direction, which is the one the retry must not break: a
// holder that is genuinely alive keeps the role, and the second replica
// says so — late, but it says so.
func TestAcquireSchedulingLease_ALiveHolderIsStillRefusedAfterTheWindow(t *testing.T) {
	t.Parallel()
	db := schedLeaseDB(t)
	ctx := context.Background()
	logger, _ := capturingLogger()

	first := acquireSchedulingLease(ctx, db, "nightly", logger)
	defer first.release()

	secondLog, secondBuf := capturingLogger()
	start := time.Now()
	second := acquireSchedulingLeaseWithin(ctx, db, "nightly", secondLog, 400*time.Millisecond, 50*time.Millisecond)
	defer second.release()

	if second.drivesScheduledWork() {
		t.Fatalf("a live holder must keep the role; log was %s", secondBuf.String())
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Errorf("gave up after %s without waiting out the window; a kill-restart would be refused this way", waited)
	}
	if got := secondBuf.String(); !strings.Contains(got, "level=ERROR") {
		t.Errorf("the eventual refusal is still an ERROR; got %s", got)
	}
}

// A cancelled boot must not sit in the retry loop. Without the ctx check
// the daemon would keep polling for the whole window after being told to
// stop.
func TestAcquireSchedulingLease_RetryStopsWhenBootIsCancelled(t *testing.T) {
	t.Parallel()
	db := schedLeaseDB(t)
	logger, _ := capturingLogger()

	first := acquireSchedulingLease(context.Background(), db, "nightly", logger)
	defer first.release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	second := acquireSchedulingLeaseWithin(ctx, db, "nightly", logger, time.Minute, 50*time.Millisecond)
	defer second.release()
	if second.drivesScheduledWork() {
		t.Error("a cancelled boot should not conclude it drives the work")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("a cancelled boot waited %s for the retry window", waited)
	}
}

// The window is sized against the staleness it is waiting out. Pinned
// because the two live in different packages: shortening the lease
// timings without this would leave a window that waits for nothing, and
// lengthening them would leave one that gives up too early.
func TestSchedLeaseRetryWindowOutlastsStaleness(t *testing.T) {
	t.Parallel()
	if schedLeaseRetryFor <= eventlog.InstanceLeaseStaleAfter {
		t.Errorf("retry window %s does not outlast the %s staleness window, so an instance booting after a kill can never take the abandoned lease",
			schedLeaseRetryFor, eventlog.InstanceLeaseStaleAfter)
	}
}

// lost() on a lease that was never held must block, not fire. A passive
// replica selecting on it would otherwise immediately "lose" a lease it
// never had and log a split-brain that is not happening.
func TestSchedulingLease_NeverHeldNeverReportsLoss(t *testing.T) {
	t.Parallel()
	db := schedLeaseDB(t)
	ctx := context.Background()
	logger, _ := capturingLogger()

	first := acquireSchedulingLease(ctx, db, "nightly", logger)
	defer first.release()
	second := acquireNow(ctx, db, "nightly", logger)
	defer second.release()

	select {
	case <-second.lost():
		t.Error("a passive replica reported losing a lease it never held")
	default:
	}
	// And the nil-store case, which takes the other branch.
	select {
	case <-acquireSchedulingLease(ctx, nil, "nightly", logger).lost():
		t.Error("a daemon with no store reported losing a lease that does not exist")
	default:
	}
}

func TestSchedulingLease_NilIsUsable(t *testing.T) {
	t.Parallel()
	var s *schedulingLease
	if s.drivesScheduledWork() {
		t.Error("a nil lease should not claim the role")
	}
	if err := s.release(); err != nil {
		t.Errorf("release on a nil lease: %v", err)
	}
	select {
	case <-s.lost():
		t.Error("a nil lease reported loss")
	default:
	}
}

func TestSchedulingLeaseName(t *testing.T) {
	t.Parallel()
	if got := schedulingLeaseName("nightly"); got != "scheduling/nightly" {
		t.Errorf("schedulingLeaseName(nightly) = %q", got)
	}
	// An unnamed workload still gets a stable key; it must not become
	// the empty string, which AcquireInstanceLease rejects.
	if got := schedulingLeaseName(""); got != "scheduling" {
		t.Errorf("schedulingLeaseName(\"\") = %q, want a stable non-empty key", got)
	}
}
