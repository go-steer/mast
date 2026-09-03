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

// The plain path's half of what sql_test.go pins for Open (#274). The
// two paths had drifted: Open injected busy_timeout AND
// _txlock=immediate, OpenSessionService only the first. That was
// survivable while the ADK service was the sole writer here —
// serializedService's mutex kept it to one at a time — and stops being
// survivable the moment a caller takes the returned *gorm.DB and writes
// its own rows outside that mutex, which is exactly what the durable
// budget ledger now does.

package eventlog

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"
)

// The DSN pin, mirroring TestOpen_InjectsImmediateTxlock. Cheap, and it
// names the missing setting rather than leaving the next reader to infer
// it from a timeout.
func TestOpenSessionService_InjectsImmediateTxlock(t *testing.T) {
	t.Parallel()
	d := sqlite.Open(filepath.Join(t.TempDir(), "plain.db"))
	if _, _, err := OpenSessionServiceWithDB(context.Background(), d); err != nil {
		t.Fatalf("OpenSessionServiceWithDB: %v", err)
	}

	dsn := dsnOf(t, d)
	if !strings.Contains(dsn, "_txlock=immediate") {
		t.Errorf("DSN %q missing _txlock=immediate; a second writer on this connection will hit an unretryable SQLITE_BUSY", dsn)
	}
	if !strings.Contains(dsn, "_pragma=busy_timeout(5000)") {
		t.Errorf("DSN %q missing busy_timeout pragma", dsn)
	}
}

// The behaviour the DSN buys, and the one that actually regresses. A
// budget-ledger row is written with raw gorm on the connection this
// function returns; it does not pass serializedService's mutex, so it can
// hold the database-wide write lock while ADK's AppendEvent — a
// read-then-write transaction — is committing. Under a deferred BEGIN
// that is a snapshot→write upgrade, which SQLite refuses with an
// immediate SQLITE_BUSY that busy_timeout deliberately never retries.
//
// Fails on pre-fix code with "database is locked (5) (SQLITE_BUSY)".
func TestOpenSessionService_AppendEventWaitsForALedgerWriteHoldingTheLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dsn := filepath.Join(t.TempDir(), "plain.db")
	svc, db, err := OpenSessionServiceWithDB(ctx, sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("OpenSessionServiceWithDB: %v", err)
	}
	if db == nil {
		t.Fatal("no DB returned for an on-disk SQLite store")
	}

	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "sess"})
	if err != nil {
		t.Fatalf("Service.Create: %v", err)
	}
	sess := resp.Session

	// Stand in for the spend ledger: a table mast owns, on the connection
	// the caller was handed. Writing to any table takes the one
	// database-wide write lock.
	if err := db.WithContext(ctx).Exec("CREATE TABLE ledger_stub (id INTEGER)").Error; err != nil {
		t.Fatalf("create ledger stub: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}

	lockAcquired := make(chan struct{})
	release := make(chan struct{})
	holdErr := make(chan error, 1)
	go func() {
		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			holdErr <- err
			close(lockAcquired)
			return
		}
		defer func() { _ = conn.Close() }()
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			holdErr <- err
			close(lockAcquired)
			return
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO ledger_stub (id) VALUES (1)"); err != nil {
			_ = tx.Rollback()
			holdErr <- err
			close(lockAcquired)
			return
		}
		close(lockAcquired)
		<-release
		holdErr <- tx.Rollback()
	}()

	<-lockAcquired
	select {
	case err := <-holdErr:
		t.Fatalf("ledger writer failed before AppendEvent: %v", err)
	default:
	}

	appendErr := make(chan error, 1)
	go func() {
		appendErr <- svc.AppendEvent(ctx, sess, makeEvent("ev-contended", "assistant", "", "diagnosis"))
	}()

	// Let AppendEvent reach its BEGIN IMMEDIATE and block, then release
	// well inside the 5s busy_timeout.
	time.Sleep(300 * time.Millisecond)
	close(release)

	select {
	case err := <-appendErr:
		if err != nil {
			t.Fatalf("AppendEvent lost a turn's event to a ledger write instead of waiting for it: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AppendEvent did not complete after the ledger write was rolled back")
	}
	if err := <-holdErr; err != nil {
		t.Logf("ledger writer rollback: %v", err)
	}
}
