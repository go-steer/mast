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
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// healthReadTimeout bounds the probe's own query. A readiness check
// that can block is a readiness check that hangs the probe endpoint,
// and kubelet's own timeout would then be the only thing bounding it —
// at which point the failure is reported as "probe timed out" rather
// than as the database being unreachable.
const healthReadTimeout = 2 * time.Second

// CheckSessionDB reports whether the session database can still answer
// a read. It is the probe behind the daemon's /healthz.
//
// It reads one row from ADK's `events` table, which is the session
// store's own event log and the table every durable path creates —
// both the eventlog-overlay path (Open) and the plain one
// (OpenSessionServiceWithDB). A zero-row result is success: an empty
// log is a daemon that has not run a turn yet, not a broken one.
//
// A real read rather than a pool ping. A ping asks the driver whether
// it believes it has a connection; it is answered without touching the
// data, and on a pooled driver it can be answered by a connection
// checked out before whatever broke. A read catches a dropped or
// corrupted table, a database locked past the busy timeout, a closed
// pool, a Postgres that has gone away, and — verified on mast's SQLite
// driver in health_test.go rather than assumed — a database file
// deleted out from under the running process.
//
// What it does not catch, stated because a health check believed to
// cover more than it does is worse than none: a read-only volume.
// Reads are exactly what still works there. The first write fails,
// which surfaces on the turn, not here.
//
// The returned error is for the log. It routinely names a filesystem
// path or a DSN, so callers must not put it in an unauthenticated
// response body.
func CheckSessionDB(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("eventlog: no session database is open")
	}
	ctx, cancel := context.WithTimeout(ctx, healthReadTimeout)
	defer cancel()
	var id string
	// Raw rather than the model: the probe must not depend on ADK's
	// struct shape, only on the table being readable.
	if err := db.WithContext(ctx).Raw("SELECT id FROM events LIMIT 1").Scan(&id).Error; err != nil {
		return fmt.Errorf("eventlog: session db read failed: %w", err)
	}
	return nil
}
