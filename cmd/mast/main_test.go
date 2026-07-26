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
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
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
	svc, err := buildSessionService("sqlite", "", discardLogger())
	if err != nil {
		t.Fatalf("buildSessionService(sqlite, \"\"): %v", err)
	}
	if svc == nil {
		t.Fatal("buildSessionService(sqlite, \"\"): nil service")
	}

	if _, err := buildSessionService("postgres", "", discardLogger()); err == nil {
		t.Fatal("buildSessionService(postgres, \"\"): want error, got nil")
	}
}

// TestBuildSessionServiceSQLite exercises the full open+migrate path
// against a temp-dir SQLite file (house rule: scratch state under the
// test temp dir, never $HOME).
func TestBuildSessionServiceSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	svc, err := buildSessionService("sqlite", path, discardLogger())
	if err != nil {
		t.Fatalf("buildSessionService(sqlite, %q): %v", path, err)
	}
	if svc == nil {
		t.Fatal("buildSessionService returned nil service")
	}
}

// No live-Postgres test on purpose: buildSessionService with a real
// postgres DSN needs a reachable server (gorm.Open pings). The driver
// selection above is the unit-testable seam; end-to-end Postgres is a
// deployment concern (examples/deploy/cloud-run).
