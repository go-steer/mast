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

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/internal/eventlog"
)

// sessionDialector maps --session-db-driver onto a GORM dialector for
// ADK's session/database service. SQLite and Postgres are the same
// one-call surface (docs/deployment-design.md, 2026-07-25 revision):
// database.NewSessionService takes either dialector identically. For
// sqlite the DSN is a file path; for postgres it is a DSN or
// postgres:// URL (Cloud Run's required shape — no persistent disk).
func sessionDialector(driver, dsn string) (gorm.Dialector, error) {
	switch driver {
	case "sqlite":
		if err := ensureSQLiteDir(dsn); err != nil {
			return nil, err
		}
		return sqlite.Open(dsn), nil
	case "postgres":
		return postgres.Open(dsn), nil
	default:
		return nil, fmt.Errorf("unknown --session-db-driver %q (want `sqlite` or `postgres`)", driver)
	}
}

// ensureSQLiteDir creates the parent directory for a SQLite file DSN.
// SQLite won't create intermediate directories and reports a missing
// parent as the cryptic "unable to open database file: out of memory
// (14)" (SQLITE_CANTOPEN) — hit on the first smoke run with
// --session-db=/tmp/mast/smoke.db before /tmp/mast existed. An
// unattended daemon's first boot must not fail on an empty state
// directory, so create it instead of demanding a clearer error from
// the operator's runbook. file: URIs are unwrapped (query params
// stripped); in-memory forms pass through untouched.
func ensureSQLiteDir(dsn string) error {
	path := dsn
	if rest, ok := strings.CutPrefix(path, "file:"); ok {
		path = strings.TrimPrefix(rest, "//")
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
	}
	if path == "" || path == ":memory:" || strings.HasPrefix(path, ":") {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "." || dir == "/" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create session-db directory %s: %w", dir, err)
	}
	return nil
}

// The returned *gorm.DB is the connection the service opened, for the
// durable stores to put their tables on; nil when sessions are in-memory
// (#274). Callers treat nil as "nothing to persist to", not as an error.
func buildSessionService(ctx context.Context, driver, dsn string, logger *slog.Logger) (session.Service, *gorm.DB, error) {
	if dsn == "" {
		if driver != "sqlite" {
			return nil, nil, fmt.Errorf("--session-db-driver=%s requires --session-db (a DSN); empty --session-db means in-memory sessions", driver)
		}
		logger.Warn("no --session-db; sessions are in-memory and will NOT survive restart")
		return session.InMemoryService(), nil, nil
	}
	dial, err := sessionDialector(driver, dsn)
	if err != nil {
		return nil, nil, err
	}
	// Same storage hardening as the attach path (write serialization
	// + busy_timeout + _txlock=immediate + WAL for SQLite) minus the
	// seq overlay — the raw service lost markers and transcript events
	// to SQLITE_BUSY under concurrent sessions (#53).
	svc, db, err := eventlog.OpenSessionServiceWithDB(ctx, dial)
	if err != nil {
		return nil, nil, fmt.Errorf("open session db (driver %s): %w", driver, err)
	}
	// Deliberately not logging the DSN: a Postgres DSN carries
	// credentials. The sqlite path is safe and useful for operators.
	if driver == "sqlite" {
		logger.Info("session db opened", "driver", driver, "path", dsn)
	} else {
		logger.Info("session db opened", "driver", driver)
	}
	return svc, db, nil
}
