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

// Originally derived from go-steer/core-agent@25d8531cf8d1d69459471009a9e7e2e9b0dff1e2

package eventlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// agentRunLockRow is the persistent lease that prevents two
// processes from simultaneously running RunAutonomous /
// ResumeAutonomous against the same (app, user, session). One row
// per locked session; presence == lease held; staleness is detected
// via heartbeat_at.
type agentRunLockRow struct {
	AppName     string `gorm:"primaryKey"`
	UserID      string `gorm:"primaryKey"`
	SessionID   string `gorm:"primaryKey"`
	Holder      string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
}

// TableName pins the table name independent of GORM's pluralization.
func (agentRunLockRow) TableName() string { return "agent_run_lock" }

// Lock-tuning constants. Heartbeat is best-effort: a 5s interval
// against a 30s staleness window leaves plenty of headroom for
// short hiccups (DB pause, GC stall) without false-sharing the
// lock.
//
// defaultHeartbeatTimeout bounds each heartbeat UPDATE so a wedged
// database can't hang the heartbeat goroutine (and, via Release's
// wait on the goroutine, Release itself) indefinitely. It comfortably
// exceeds SQLite's 5s busy_timeout so a heartbeat isn't spuriously
// cancelled while merely waiting on the write lock.
const (
	defaultHeartbeatInterval = 5 * time.Second
	defaultStaleAfter        = 30 * time.Second
	defaultHeartbeatTimeout  = 10 * time.Second
)

// An instance lease beats faster and goes stale sooner than a session
// lock, because the two get the trade-off from opposite ends.
//
// Stealing a session lock too eagerly means two processes running one
// session — the split-brain the lock exists to prevent — so 30s of
// tolerance for a GC pause is cheap. Stealing an *instance* lease too
// eagerly costs a few seconds of two daemons both scheduling, which is
// merely the state mast was in before the lease existed. Waiting too
// long costs much more: a daemon that was SIGKILLed (OOM, node
// eviction) leaves a lease nothing released, and until that lease goes
// stale its own replacement is refused — and a refused replacement
// stays passive for life, so a 30s window would mean an OOM kill
// silently ends scheduled work until someone restarts the pod a second
// time. Eight seconds bounds that recovery; it also bounds how long a
// genuinely-second replica spends waiting at boot before concluding it
// is second (see cmd/mast/schedlease.go).
const (
	instanceHeartbeatInterval = 2 * time.Second
	// InstanceLeaseStaleAfter is how long an instance lease survives its
	// holder's last heartbeat. Exported because a caller retrying an
	// acquisition has to size its retry window against it.
	InstanceLeaseStaleAfter = 8 * time.Second
)

// leaseTiming is the heartbeat/staleness triple a lease runs on.
type leaseTiming struct {
	heartbeat time.Duration
	stale     time.Duration
	timeout   time.Duration
}

func sessionTiming() leaseTiming {
	return leaseTiming{defaultHeartbeatInterval, defaultStaleAfter, defaultHeartbeatTimeout}
}

func instanceTiming() leaseTiming {
	return leaseTiming{instanceHeartbeatInterval, InstanceLeaseStaleAfter, defaultHeartbeatTimeout}
}

// ErrSessionLocked is returned by AcquireLock when another live
// process already holds the lease. The error message includes the
// holder identifier so operators can diagnose contention.
var ErrSessionLocked = errors.New("eventlog: session is locked by another process")

// SessionLock is the lease returned by Handle.AcquireLock. It runs a
// background goroutine that refreshes heartbeat_at every
// heartbeatInterval until Release is called. Safe to call Release
// multiple times.
type SessionLock struct {
	db        *gorm.DB
	app, user string
	session   string
	holder    string

	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
	staleAfter        time.Duration

	mu       sync.Mutex
	released bool
	stop     chan struct{}
	done     chan struct{}
	// lost is closed by the heartbeat loop if it discovers the lease
	// was stolen out from under us (its conditional UPDATE matched zero
	// rows). Callers running work under the lock select on Lost() to
	// abort before committing split-brain writes.
	lost chan struct{}
}

// AcquireLock takes an exclusive lease on (app, user, session) for
// the lifetime of the returned *SessionLock. Returns ErrSessionLocked
// if another process holds a fresh lease (heartbeat within
// staleAfter); steals the lease if the existing holder's heartbeat
// is older than staleAfter (indicating a crashed process).
//
// The lease is heartbeated automatically until SessionLock.Release
// is called; Release is idempotent and safe to defer.
func (h *Handle) AcquireLock(ctx context.Context, app, user, session string) (*SessionLock, error) {
	if h == nil || h.db == nil {
		return nil, errors.New("eventlog: AcquireLock called on nil Handle")
	}
	return acquireLock(ctx, h.db, app, user, session, sessionTiming())
}

// InstanceLease is a SessionLock held at the fleet grain rather than
// the session grain — same row, same heartbeat, same steal-on-stale
// rule, different question. A SessionLock answers "may this process run
// *this session* right now"; an InstanceLease answers "is this process
// the one that drives unrequested work against this store".
type InstanceLease = SessionLock

// The sentinel (app, user) an instance lease occupies. Colons are the
// same guard `mast:scheduler` uses for a caller identity: no ADK app
// name or user id mast mints can take this form, so a fleet-grain row
// can never collide with a session-grain one even though they share the
// table. Sharing the table is deliberate — one row shape, one heartbeat
// implementation, one set of staleness semantics to reason about.
const (
	instanceLeaseApp  = "mast:instance"
	instanceLeaseUser = "mast:lease"
)

// AcquireInstanceLease takes the named fleet-grain lease on db, so that
// exactly one live process at a time considers itself responsible for
// work nobody asked for — scheduled triggers, timed-pause resumes, any
// cadence that would otherwise run once per replica.
//
// Returns ErrSessionLocked, naming the holder, when another live
// process has it. Steals a lease whose holder stopped heartbeating more
// than staleAfter ago, which is what makes a crashed leader recoverable
// without an operator.
//
// It takes a *gorm.DB rather than a *Handle because the leases that
// matter are held by daemons that never opened the attach overlay —
// same reason NewGuardrailStore and NewSpendStore take one.
//
// This does NOT make mast multi-replica. It makes the second replica
// *quiet*: it stops that replica duplicating timed work, and it does
// not hand over the work if the leader goes away and a passive replica
// is already running (see cmd/mast/schedlease.go for what the daemon
// does with the refusal, and docs/deployment-design.md for the part
// that is still designed and not built).
func AcquireInstanceLease(ctx context.Context, db *gorm.DB, name string) (*InstanceLease, error) {
	if db == nil {
		return nil, errors.New("eventlog: AcquireInstanceLease called with no database")
	}
	if name == "" {
		return nil, errors.New("eventlog: AcquireInstanceLease needs a lease name")
	}
	return acquireLock(ctx, db, instanceLeaseApp, instanceLeaseUser, name, instanceTiming())
}

func acquireLock(ctx context.Context, db *gorm.DB, app, user, session string, t leaseTiming) (*SessionLock, error) {
	if err := db.WithContext(ctx).AutoMigrate(&agentRunLockRow{}); err != nil {
		return nil, fmt.Errorf("eventlog: AutoMigrate agent_run_lock: %w", err)
	}
	holder := newHolderID()
	now := time.Now()
	row := &agentRunLockRow{
		AppName:     app,
		UserID:      user,
		SessionID:   session,
		Holder:      holder,
		AcquiredAt:  now,
		HeartbeatAt: now,
	}

	// Try to insert. If the row already exists, GORM surfaces the
	// unique-constraint violation; we then check for staleness.
	//
	// The insert runs with GORM's own logger silenced because for an
	// instance lease the collision is the *expected* outcome on every
	// replica but one (#345), and GORM would print the raw constraint
	// failure and the INSERT above the caller's explanation of it —
	// making a correctly-configured passive replica look like it hit a
	// database fault at boot. Nothing is lost: the error is still
	// returned here, and every path below either classifies it
	// (ErrSessionLocked) or wraps and surfaces it.
	err := db.WithContext(ctx).Session(&gorm.Session{Logger: gormlogger.Discard}).Create(row).Error
	if err != nil {
		// Slow path: existing row. Check whether it's stale.
		var existing agentRunLockRow
		if lookupErr := db.WithContext(ctx).
			Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, session).
			First(&existing).Error; lookupErr != nil {
			// Couldn't even read the row; surface the original
			// insert error.
			return nil, fmt.Errorf("eventlog: acquire lease: %w", err)
		}
		if time.Since(existing.HeartbeatAt) <= t.stale {
			return nil, fmt.Errorf("%w (held by %s, last heartbeat %s ago)",
				ErrSessionLocked, existing.Holder, time.Since(existing.HeartbeatAt).Round(time.Second))
		}
		// Steal: overwrite the stale lease with our identity in a
		// single UPDATE. Predicate keeps the operation safe under
		// concurrent stealers — only one update succeeds, the
		// other rebounds to the locked path on its next attempt.
		res := db.WithContext(ctx).
			Model(&agentRunLockRow{}).
			Where("app_name = ? AND user_id = ? AND session_id = ? AND holder = ?",
				app, user, session, existing.Holder).
			Updates(map[string]any{
				"holder":       holder,
				"acquired_at":  now,
				"heartbeat_at": now,
			})
		if res.Error != nil {
			return nil, fmt.Errorf("eventlog: acquire lease: steal stale lease: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// Someone else stole first.
			return nil, fmt.Errorf("%w (lost the steal race)", ErrSessionLocked)
		}
	}

	lock := &SessionLock{
		db:                db,
		app:               app,
		user:              user,
		session:           session,
		holder:            holder,
		heartbeatInterval: t.heartbeat,
		heartbeatTimeout:  t.timeout,
		staleAfter:        t.stale,
		stop:              make(chan struct{}),
		done:              make(chan struct{}),
		lost:              make(chan struct{}),
	}
	go lock.heartbeatLoop()
	return lock, nil
}

// Release ends the lease and stops the heartbeat goroutine.
// Idempotent; safe to defer.
func (l *SessionLock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	close(l.stop)
	l.mu.Unlock()

	// Wait for the heartbeat goroutine to exit before deleting the
	// row so we don't race a heartbeat against the delete.
	<-l.done

	// Conditional delete: only remove the row if we still hold it
	// (i.e. nobody stole it after a stale-window elapsed during a
	// long pause). Avoids accidentally deleting a successor's
	// lease.
	res := l.db.
		Where("app_name = ? AND user_id = ? AND session_id = ? AND holder = ?",
			l.app, l.user, l.session, l.holder).
		Delete(&agentRunLockRow{})
	if res.Error != nil {
		return fmt.Errorf("eventlog: SessionLock.Release: %w", res.Error)
	}
	return nil
}

// Holder returns the identifier we registered when acquiring the
// lock. Useful for diagnostics + for tests that need to assert the
// row content.
func (l *SessionLock) Holder() string { return l.holder }

// Lost returns a channel that is closed if the lease is stolen out
// from under us while it is held — the heartbeat's conditional UPDATE
// matched zero rows, meaning another process reclaimed the lease after
// our heartbeat lapsed past the staleness window (a >staleAfter GC
// pause, sleep, or DB stall). A caller running work under the lock —
// e.g. the autonomous run loop — must select on this channel and abort
// promptly, otherwise both processes run against the same session: the
// exact split-brain the lock exists to prevent. The channel is never
// closed for a lock that is cleanly Released while still held.
func (l *SessionLock) Lost() <-chan struct{} { return l.lost }

func (l *SessionLock) heartbeatLoop() {
	defer close(l.done)
	ticker := time.NewTicker(l.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			if l.beat() {
				// Lease lost. Signal any consumer running under the
				// lock so it can abort, then stop heartbeating —
				// continuing to refresh a row we no longer own is
				// pointless.
				close(l.lost)
				return
			}
		}
	}
}

// beat refreshes heartbeat_at for the row we still own, bounded by
// heartbeatTimeout so a wedged database can't hang the loop forever. It
// returns true only when the lease has been definitively lost — the
// conditional UPDATE succeeded but matched zero rows, meaning our
// holder no longer owns the row. A transient error (context timeout, DB
// hiccup) is deliberately NOT treated as loss: the lease may still be
// ours, so we return false and retry on the next tick.
func (l *SessionLock) beat() (lost bool) {
	ctx, cancel := context.WithTimeout(context.Background(), l.heartbeatTimeout)
	defer cancel()
	res := l.db.WithContext(ctx).
		Model(&agentRunLockRow{}).
		Where("app_name = ? AND user_id = ? AND session_id = ? AND holder = ?",
			l.app, l.user, l.session, l.holder).
		Update("heartbeat_at", time.Now())
	if res.Error != nil {
		return false
	}
	return res.RowsAffected == 0
}

// newHolderID builds a per-acquisition identifier string of the
// form "<host>/<pid>/<rand>" so logs and diagnostic messages can
// trace the holding process across the cluster.
func newHolderID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}
