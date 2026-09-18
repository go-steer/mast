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

// The fleet-grain half of the lease (#345). The session-grain
// behaviour — heartbeat, steal-on-stale, conditional release — is
// covered in lock_test.go and shared verbatim; what is tested here is
// what AcquireInstanceLease adds: it works off a plain *gorm.DB with no
// Handle, it keys on a name, and its rows cannot be confused with a
// session's.
package eventlog

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// leaseDB returns an on-disk SQLite connection with no Handle over it —
// which is the point. A daemon that never attached has no *Handle, and
// before #345 the lock primitive could only be reached through one.
func leaseDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "lease.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	return db
}

// The headline: two daemons, one store, one name — exactly one of them
// believes it drives the work, and the loser is told who won.
func TestAcquireInstanceLease_SecondInstanceIsRefused(t *testing.T) {
	t.Parallel()
	db := leaseDB(t)
	ctx := context.Background()

	first, err := AcquireInstanceLease(ctx, db, "scheduling/nightly")
	if err != nil {
		t.Fatalf("first AcquireInstanceLease: %v", err)
	}
	defer first.Release()

	second, err := AcquireInstanceLease(ctx, db, "scheduling/nightly")
	if !errors.Is(err, ErrSessionLocked) {
		if err == nil {
			second.Release()
		}
		t.Fatalf("second AcquireInstanceLease err = %v, want ErrSessionLocked", err)
	}
	if !strings.Contains(err.Error(), first.Holder()) {
		t.Errorf("the refusal must name the holder so an operator can find the other pod; got %v", err)
	}
}

// Keyed on the name, not global: two daemons running different
// workloads against one store are not duplicating each other's
// schedules, and refusing that would be a claim the lease has no
// evidence for.
func TestAcquireInstanceLease_DifferentNamesDoNotBlock(t *testing.T) {
	t.Parallel()
	db := leaseDB(t)
	ctx := context.Background()

	a, err := AcquireInstanceLease(ctx, db, "scheduling/nightly")
	if err != nil {
		t.Fatalf("AcquireInstanceLease(nightly): %v", err)
	}
	defer a.Release()
	b, err := AcquireInstanceLease(ctx, db, "scheduling/hourly")
	if err != nil {
		t.Fatalf("AcquireInstanceLease(hourly) should not be blocked by nightly: %v", err)
	}
	defer b.Release()
}

// A clean shutdown hands over immediately rather than making the
// successor wait out the 30s staleness window.
func TestAcquireInstanceLease_ReleaseHandsOverAtOnce(t *testing.T) {
	t.Parallel()
	db := leaseDB(t)
	ctx := context.Background()

	first, err := AcquireInstanceLease(ctx, db, "scheduling/w")
	if err != nil {
		t.Fatalf("first AcquireInstanceLease: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := AcquireInstanceLease(ctx, db, "scheduling/w")
	if err != nil {
		t.Fatalf("re-acquire after Release: %v", err)
	}
	defer second.Release()
	if second.Holder() == first.Holder() {
		t.Errorf("successor should have its own holder id; both are %q", second.Holder())
	}
}

// The crash path, and the reason schedlease.go can say Kubernetes
// restores the leader: a holder that stopped heartbeating loses the
// lease to the next process without an operator touching anything.
func TestAcquireInstanceLease_StaleLeaderIsReplaced(t *testing.T) {
	t.Parallel()
	db := leaseDB(t)
	ctx := context.Background()

	if err := db.WithContext(ctx).AutoMigrate(&agentRunLockRow{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	const dead = "crashed-pod/1/dead"
	if err := db.WithContext(ctx).Create(&agentRunLockRow{
		AppName:     instanceLeaseApp,
		UserID:      instanceLeaseUser,
		SessionID:   "scheduling/w",
		Holder:      dead,
		AcquiredAt:  time.Now().Add(-time.Hour),
		HeartbeatAt: time.Now().Add(-time.Hour),
	}).Error; err != nil {
		t.Fatalf("plant a stale leader: %v", err)
	}

	lease, err := AcquireInstanceLease(ctx, db, "scheduling/w")
	if err != nil {
		t.Fatalf("a stale leader must not block its replacement; got %v", err)
	}
	defer lease.Release()
	if lease.Holder() == dead {
		t.Errorf("replacement kept the dead holder id %q", dead)
	}
}

// The two grains share one table, so the sentinel (app, user) has to be
// unreachable from the session grain. If it were not, a session id that
// happened to equal a lease name would silently lock out the scheduler
// — or be locked out by it.
func TestAcquireInstanceLease_DoesNotCollideWithASessionLock(t *testing.T) {
	t.Parallel()
	db := leaseDB(t)
	ctx := context.Background()

	lease, err := AcquireInstanceLease(ctx, db, "scheduling/w")
	if err != nil {
		t.Fatalf("AcquireInstanceLease: %v", err)
	}
	defer lease.Release()

	// Same string in the session slot, from a workload that picked an
	// unlucky session id.
	h := &Handle{db: db}
	sess, err := h.AcquireLock(ctx, "mast", "operator", "scheduling/w")
	if err != nil {
		t.Fatalf("a session named like a lease must still be lockable: %v", err)
	}
	defer sess.Release()

	// And the reverse direction: the session lock did not take the
	// lease's row with it.
	var row agentRunLockRow
	if err := db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", instanceLeaseApp, instanceLeaseUser, "scheduling/w").
		First(&row).Error; err != nil {
		t.Fatalf("the lease row should be untouched: %v", err)
	}
	if row.Holder != lease.Holder() {
		t.Errorf("lease row holder = %q, want %q", row.Holder, lease.Holder())
	}
}

// A passive replica is a correctly configured replica, so its boot must
// not look like a database fault. GORM logs a failed statement itself,
// at Error level, before the caller ever sees the error — which for the
// expected collision would print the raw UNIQUE constraint failure and
// the INSERT above the daemon's own explanation of what happened.
func TestAcquireInstanceLease_TheExpectedCollisionIsNotLoggedAsAFault(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "lease.db")), &gorm.Config{
		Logger: gormlogger.New(log.New(&out, "", 0), gormlogger.Config{LogLevel: gormlogger.Error}),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	ctx := context.Background()

	first, err := AcquireInstanceLease(ctx, db, "scheduling/w")
	if err != nil {
		t.Fatalf("first AcquireInstanceLease: %v", err)
	}
	defer first.Release()
	out.Reset()

	if _, err := AcquireInstanceLease(ctx, db, "scheduling/w"); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("second AcquireInstanceLease err = %v, want ErrSessionLocked", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("the refusal printed a database fault the operator did not cause:\n%s", got)
	}
}

func TestAcquireInstanceLease_RejectsAnUnusableRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if _, err := AcquireInstanceLease(ctx, nil, "scheduling/w"); err == nil {
		t.Error("a nil database should be an error, not a lease nobody holds")
	}
	if _, err := AcquireInstanceLease(ctx, leaseDB(t), ""); err == nil {
		t.Error("an empty name should be an error; it would otherwise be a second global singleton")
	}
}
