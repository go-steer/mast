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
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func healthDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	_, db, err := OpenSessionServiceWithDB(context.Background(), sqlite.Open(path))
	if err != nil {
		t.Fatalf("OpenSessionServiceWithDB: %v", err)
	}
	if db == nil {
		t.Fatal("no DB returned for an on-disk SQLite store")
	}
	return db, path
}

// TestCheckSessionDB_EmptyLogIsReady: a daemon that has not run a turn
// yet is ready. If zero rows read as an error the probe would hold
// every pod unready until its first inject, which is the opposite of
// what a readiness gate is for.
func TestCheckSessionDB_EmptyLogIsReady(t *testing.T) {
	t.Parallel()
	db, _ := healthDB(t)
	if err := CheckSessionDB(context.Background(), db); err != nil {
		t.Errorf("CheckSessionDB on a fresh store = %v, want nil", err)
	}
}

// TestCheckSessionDB_NoDatabase covers the in-memory daemon: there is
// nothing durable to read, and the probe says so rather than passing.
// The caller decides whether that is a red state — cmd/mast reports no
// session_db check at all rather than a failing one.
func TestCheckSessionDB_NoDatabase(t *testing.T) {
	t.Parallel()
	if err := CheckSessionDB(context.Background(), nil); err == nil {
		t.Error("CheckSessionDB(nil) = nil, want an error")
	}
}

// TestCheckSessionDB_MissingTable is the shape a pool ping cannot see:
// the connection is fine and the data is not there. Dropping the table
// stands in for the family — a truncated file, a restore that did not
// finish, a migration that ran against the wrong database.
func TestCheckSessionDB_MissingTable(t *testing.T) {
	t.Parallel()
	db, _ := healthDB(t)
	if err := db.Exec("DROP TABLE events").Error; err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}
	if err := CheckSessionDB(context.Background(), db); err == nil {
		t.Error("CheckSessionDB after DROP TABLE events = nil, want an error")
	}
}

// TestCheckSessionDB_ClosedPool: the process still holds a *gorm.DB but
// the pool behind it is gone. A handler that only nil-checked its
// dependency would report ready here.
func TestCheckSessionDB_ClosedPool(t *testing.T) {
	t.Parallel()
	db, _ := healthDB(t)
	if err := closeGormDB(db); err != nil {
		t.Fatalf("close pool: %v", err)
	}
	if err := CheckSessionDB(context.Background(), db); err == nil {
		t.Error("CheckSessionDB against a closed pool = nil, want an error")
	}
}

// TestCheckSessionDB_UnlinkedFile is the case the filing named: the
// session database is deleted out from under a running daemon.
//
// The reasoning to distrust here is "POSIX keeps the descriptor valid
// after unlink, so SQLite keeps serving and the read cannot see it."
// That is true of a process holding a plain file and false here, which
// is why this is a test and not a comment: mast's driver is pure-Go
// (glebarez/sqlite over modernc), the store runs in WAL mode, and the
// read comes back `disk I/O error (1802)`.
//
// So the probe does catch it — on this driver. The assertion is that it
// fails, not on which error, because the message is the driver's and a
// driver swap is allowed to change it. If this test ever starts
// failing, the honest response is to weaken CheckSessionDB's doc
// comment, not to reach for a stat() on the DSN: that check has its own
// false positives (an atomic replace, a bind-mount remount).
func TestCheckSessionDB_UnlinkedFile(t *testing.T) {
	t.Parallel()
	db, path := healthDB(t)
	if err := os.Remove(path); err != nil {
		t.Fatalf("unlink the database: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("database still present after unlink: %v", err)
	}
	if err := CheckSessionDB(context.Background(), db); err == nil {
		t.Error("CheckSessionDB against a deleted database file = nil, want an error; " +
			"this is the case a pool ping cannot see and the reason the probe reads")
	}
}

// TestCheckSessionDB_CancelledContext: the probe honours its caller's
// deadline instead of outliving the request that started it.
func TestCheckSessionDB_CancelledContext(t *testing.T) {
	t.Parallel()
	db, _ := healthDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckSessionDB(ctx, db); err == nil {
		t.Error("CheckSessionDB with a cancelled context = nil, want an error")
	}
}
