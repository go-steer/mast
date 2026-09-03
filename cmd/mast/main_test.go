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

package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/eventlog"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSessionDialector verifies the --session-db-driver → dialector
// mapping. Construct-only: gorm dialectors do not touch the network
// (or the filesystem) until gorm.Open, so no live Postgres is needed.
func TestSessionDialector(t *testing.T) {
	tests := []struct {
		driver   string
		dsn      string
		wantName string
		wantErr  string
	}{
		{driver: "sqlite", dsn: "/tmp/mast-test/sessions.db", wantName: "sqlite"},
		{driver: "postgres", dsn: "postgres://mast:secret@10.0.0.5:5432/mast?sslmode=require", wantName: "postgres"},
		{driver: "postgres", dsn: "host=/cloudsql/proj:region:inst user=mast dbname=mast", wantName: "postgres"},
		{driver: "firestore", dsn: "whatever", wantErr: "unknown --session-db-driver"},
		{driver: "", dsn: "whatever", wantErr: "unknown --session-db-driver"},
	}
	for _, tt := range tests {
		dial, err := sessionDialector(tt.driver, tt.dsn)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("sessionDialector(%q, ...): got err %v, want containing %q", tt.driver, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("sessionDialector(%q, ...): unexpected error: %v", tt.driver, err)
			continue
		}
		if got := dial.Name(); got != tt.wantName {
			t.Errorf("sessionDialector(%q, ...).Name() = %q, want %q", tt.driver, got, tt.wantName)
		}
	}
}

// TestBuildSessionServiceInMemory: empty --session-db falls back to
// in-memory sessions under the default driver only — an explicit
// postgres driver with no DSN is an operator mistake, not a silent
// in-memory downgrade (that would falsify Cloud Run durability).
func TestBuildSessionServiceInMemory(t *testing.T) {
	svc, db, err := buildSessionService(context.Background(), "sqlite", "", discardLogger())
	if err != nil {
		t.Fatalf("buildSessionService(sqlite, \"\"): %v", err)
	}
	if svc == nil {
		t.Fatal("buildSessionService(sqlite, \"\"): nil service")
	}
	// In-memory is the one configuration with nothing to persist to, so
	// it is also the one that must report no connection: the durable
	// stores key on this being nil (#274), and a non-nil handle here
	// would wire a ledger onto a database that evaporates.
	if db != nil {
		t.Error("buildSessionService(sqlite, \"\"): got a DB for in-memory sessions, want nil")
	}

	if _, _, err := buildSessionService(context.Background(), "postgres", "", discardLogger()); err == nil {
		t.Fatal("buildSessionService(postgres, \"\"): want error, got nil")
	}
}

// TestBuildSessionServiceSQLite exercises the full open+migrate path
// against a temp-dir SQLite file (house rule: scratch state under the
// test temp dir, never $HOME).
func TestBuildSessionServiceSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	svc, db, err := buildSessionService(context.Background(), "sqlite", path, discardLogger())
	if err != nil {
		t.Fatalf("buildSessionService(sqlite, %q): %v", path, err)
	}
	if svc == nil {
		t.Fatal("buildSessionService returned nil service")
	}
	// The other half of #274: with a DSN there is a connection, and the
	// caller gets it. Everything about a durable ledger on the no-attach
	// path rests on this not being nil.
	if db == nil {
		t.Fatal("buildSessionService returned no DB for an on-disk session store")
	}
	// And it is usable for a table mast owns, which is the actual point —
	// NewSpendStore AutoMigrates on the connection it is handed.
	if _, err := eventlog.NewSpendStore(context.Background(), db); err != nil {
		t.Fatalf("NewSpendStore on the plain path's DB: %v", err)
	}
}

// No live-Postgres test on purpose: buildSessionService with a real
// postgres DSN needs a reachable server (gorm.Open pings). The driver
// selection above is the unit-testable seam; end-to-end Postgres is a
// deployment concern (examples/deploy/cloud-run).

// TestMisplacedFlag pins the trailing-flag guard: defined flags after
// the positional prompt are refused (Go flag parsing would silently
// feed them to the model as prompt text — hit live twice on
// 2026-07-29 with a trailing --session-db), while prompt words that
// merely look flag-ish pass through.
func TestMisplacedFlag(t *testing.T) {
	t.Parallel()
	defined := func(name string) bool {
		switch name {
		case "session-db", "task", "provider", "model":
			return true
		}
		return false
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"trailing --session-db=path", []string{"What is the latest Go release?", "--session-db=/tmp/mast/smoke.db"}, "--session-db=/tmp/mast/smoke.db"},
		{"trailing bare --task", []string{"prompt", "--task", "debug"}, "--task"},
		{"single-dash form", []string{"prompt", "-provider=gemini"}, "-provider=gemini"},
		{"quoted prompt is one clean arg", []string{"explain the --session-db flag to me"}, ""},
		{"flag-ish word that is not a defined flag", []string{"summarize", "-race", "output"}, ""},
		{"no args", nil, ""},
		{"bare dashes ignored", []string{"prompt", "--"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := misplacedFlag(tc.args, defined); got != tc.want {
				t.Errorf("misplacedFlag(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
