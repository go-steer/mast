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
	"fmt"
	"sync"

	"google.golang.org/adk/v2/session"
	adkdatabase "google.golang.org/adk/v2/session/database"
	"gorm.io/gorm"
)

// OpenSessionService constructs ADK's database session service with
// the same SQLite write hardening Open applies — busy_timeout on
// every connection, WAL journal mode, and write serialization — but
// WITHOUT the seq overlay (no agent_eventlog table, no Stream). It is
// the session backend for daemons that don't serve the attach
// surface.
//
// This exists because the hardening must not be attach-only (issue
// #53): the plain path previously opened raw SQLite, and concurrent
// sessions lost transcript events and shutdown interruption markers
// to immediate SQLITE_BUSY failures — lock-upgrade conflicts inside
// an open transaction return busy without consulting busy_timeout, so
// the write mutex is the piece that actually prevents them, and it
// lived only in Open's wrapped service. Sharing the building blocks
// here keeps the two paths from drifting.
//
// For non-SQLite dialectors (Postgres), pragmas are skipped and no
// write mutex is imposed — MVCC handles concurrent writers, and
// serializing them would only cost throughput.
func OpenSessionService(ctx context.Context, dialector gorm.Dialector) (session.Service, error) {
	svc, _, err := OpenSessionServiceWithDB(ctx, dialector)
	return svc, err
}

// OpenSessionServiceWithDB is OpenSessionService plus the connection it
// opened, for callers that put their own tables on the same database —
// the durable budget ledger (#274) is the first, as SessionACLStore is
// on Open's side.
//
// Returning it is what lets spend durability key on "there is a
// database" rather than on "the attach surface is bound", which was
// never the condition it needed: a ledger is not a latch, so unlike the
// guardrail halt it has nothing an operator must be able to clear. See
// cmd/mast's wiring for the split.
//
// The DB is ADK's own handle, read reflectively, so a caller's writes
// land on the connection the session service is already using. Nil for
// a dialector whose service does not expose one, which callers must
// treat as "no durable store here" rather than as an error — the
// session service itself is still perfectly good.
func OpenSessionServiceWithDB(ctx context.Context, dialector gorm.Dialector) (session.Service, *gorm.DB, error) {
	sqlite := isSQLite(dialector)
	if sqlite {
		injectSQLitePragma(dialector, fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMs))
		// Both of Open's settings, not just the first. The drift was
		// invisible while the ADK service was the only writer on this
		// path — serializedService's mutex kept it to one at a time —
		// and stops being invisible the moment a caller takes the DB
		// above and writes its own rows outside that mutex. Per Open's
		// comment: busy_timeout alone does nothing for concurrent
		// writers, because SQLite answers a snapshot upgrade with an
		// immediate SQLITE_BUSY that busy_timeout deliberately never
		// retries. IMMEDIATE takes the write lock up front and turns
		// that into an ordinary wait. ADK's AppendEvent is exactly the
		// read-then-write transaction that needs it.
		injectSQLiteDSNParam(dialector, "_txlock", "immediate")
	}
	svc, err := adkdatabase.NewSessionService(dialector, defaultGormOpts(defaultOpenOpts())...)
	if err != nil {
		return nil, nil, fmt.Errorf("eventlog: open ADK session service: %w", err)
	}
	if err := adkdatabase.AutoMigrate(svc); err != nil {
		return nil, nil, fmt.Errorf("eventlog: ADK AutoMigrate: %w", err)
	}
	db := adkGormDB(svc)
	if !sqlite {
		return svc, db, nil
	}
	// WAL via the service's own connection (reflective handle — same
	// mechanism Handle.Close uses). Best-effort like Open's step 3:
	// some SQLite forms (:memory:) reject WAL.
	if db != nil {
		_ = db.WithContext(ctx).Exec("PRAGMA journal_mode=WAL").Error
	}
	return &serializedService{inner: svc}, db, nil
}

// serializedService funnels all writes through one mutex — the
// SQLite single-writer discipline Open's wrapped service applies,
// minus the overlay. Reads pass through.
type serializedService struct {
	inner   session.Service
	writeMu sync.Mutex
}

func (s *serializedService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	return s.inner.Get(ctx, req)
}

func (s *serializedService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	return s.inner.List(ctx, req)
}

func (s *serializedService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inner.Create(ctx, req)
}

func (s *serializedService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inner.Delete(ctx, req)
}

func (s *serializedService) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inner.AppendEvent(ctx, sess, ev)
}
