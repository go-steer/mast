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
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// defaultDrainBound is the shutdown drain window when no workload
// budget supplies a turn ceiling (docs/durable-execution-design.md,
// "Shutdown contract"). Deliberately modest: without a budget there is
// no upper bound on how long a turn could run, and an unbounded drain
// invites SIGKILL-mid-teardown from the supervisor.
const defaultDrainBound = 30 * time.Second

// storeWriteTimeout bounds the tracker's marker writes so a wedged
// session store cannot stall turn completion or the drain itself.
const storeWriteTimeout = 10 * time.Second

// drainBound returns how long a shutdown waits for in-flight turns.
// When the workload sets budget.max_wallclock_seconds, every turn
// (inject, attach, and resume alike) is bounded by it, so the drain
// bound IS that ceiling — a finishing turn is never cut shorter than
// its own budget allows, and the worst-case drain is known at deploy
// time (size terminationGracePeriodSeconds / TimeoutStopSec above
// it). Without a budget, turns are unbounded and the default is a
// hard cut at 30s.
func drainBound(bundle *workload.Bundle) time.Duration {
	if bundle != nil && bundle.Budget.MaxWallclockSeconds > 0 {
		return time.Duration(bundle.Budget.MaxWallclockSeconds) * time.Second
	}
	return defaultDrainBound
}

// turnTracker knows which sessions have a turn in flight and owns the
// interruption-marker bookkeeping for shutdown (#39):
//
//   - beginDrain pre-marks every in-flight session durably BEFORE the
//     drain waits — a SIGKILL mid-drain leaves the markers on disk.
//   - a turn that ends while draining clears its session's marker: the
//     log then honestly records a completed (or turn-level-failed)
//     turn, not an interrupted one.
//   - freeze() stops the clearing. It runs when the drain window
//     elapses, just before the daemon cancels the surviving turns'
//     contexts — those turns ARE interrupted, and their unwinding
//     must not scrub the marker that says so.
//
// Locking discipline (#55): mu guards the counters; writeMu serializes
// each mark/clear DECISION together with its store write, with the
// decision re-checked under writeMu. Without that pairing, a turn
// finishing while beginDrain was still writing could land its clear
// BEFORE the mark — last-write-wins then reported a cleanly-finished
// session as interrupted. writeMu is never held while acquiring mu's
// critical sections' callers (mu is only taken inside writeMu, never
// the reverse).
//
// The zero tracker is not usable; construct via newTurnTracker.
// The tracker always operates as defaultUserID — the daemon's only
// writer identity; a per-tracker user would be dead configurability.
type turnTracker struct {
	store  *transcript.Store
	logger *slog.Logger

	mu       sync.Mutex
	active   map[string]int  // sessionID → in-flight turn count
	marked   map[string]bool // sessions with a durably-written, uncleared marker
	draining bool
	frozen   bool

	writeMu sync.Mutex // serializes mark/clear decision + store write
}

func newTurnTracker(store *transcript.Store, logger *slog.Logger) *turnTracker {
	return &turnTracker{
		store:  store,
		logger: logger,
		active: map[string]int{},
		marked: map[string]bool{},
	}
}

// begin records a turn starting on sessionID. A turn that starts while
// a drain is already underway is marked immediately — it may not get
// to finish, and the marker ordering must hold for it too. Marker
// writes never ride a turn's own context (which may already be
// cancelled when the bookkeeping runs).
func (t *turnTracker) begin(sessionID string) {
	t.mu.Lock()
	t.active[sessionID]++
	mark := t.draining && !t.frozen && !t.marked[sessionID]
	t.mu.Unlock()
	if mark {
		t.mark(context.Background(), sessionID)
	}
}

// end records a turn finishing on sessionID, clearing the session's
// interruption marker when this was its last in-flight turn and the
// tracker is not frozen.
//
// The clear ATTEMPT is gated only on "draining and last turn ended" —
// deliberately NOT on marked[sessionID], which is what reintroduced
// the false-interrupted bug (#60): a turn finishing during mark's
// store write read marked=false here, skipped the clear, and the mark
// then landed on a finished session forever. clear() queues behind
// writeMu, so any in-flight mark completes (and flips marked) before
// clear's own re-check runs.
func (t *turnTracker) end(sessionID string) {
	t.mu.Lock()
	t.active[sessionID]--
	if t.active[sessionID] <= 0 {
		delete(t.active, sessionID)
	}
	maybeClear := t.draining && !t.frozen && t.active[sessionID] == 0
	t.mu.Unlock()
	if maybeClear {
		t.clear(sessionID)
	}
}

// mark durably writes the interruption marker for sessionID, holding
// writeMu across the decision re-check AND the write so a racing
// clear cannot be reordered ahead of it. marked[sid] flips true only
// after the write actually landed — a failed write must not suppress
// a later attempt or trigger a phantom clear.
func (t *turnTracker) mark(ctx context.Context, sessionID string) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	skip := t.frozen || t.marked[sessionID] || t.active[sessionID] == 0
	t.mu.Unlock()
	if skip {
		// Already marked, frozen, or the turn finished while we
		// queued behind writeMu — nothing to record.
		return
	}
	// Per-write cap nested under the caller's deadline: beginDrain
	// passes the drain context, so N wedged writes cannot overrun the
	// drain window and eat the SIGKILL headroom (#63).
	ctx, cancel := context.WithTimeout(ctx, storeWriteTimeout)
	defer cancel()
	if err := t.store.MarkInterrupted(ctx, defaultUserID, sessionID, "daemon shutdown"); err != nil {
		// Error, not Warn (#53): a lost marker means an interrupted
		// turn a restart will never surface. A counter belongs here
		// too — that is the v0.2 fixed-registry work (#50).
		t.logger.Error("failed to write interruption marker", "session", sessionID, "error", err.Error())
		return
	}
	t.mu.Lock()
	t.marked[sessionID] = true
	t.mu.Unlock()
}

// clear resolves sessionID's marker after a clean finish, under the
// same writeMu so it is strictly ordered after any in-flight mark —
// by the time clear holds writeMu, a racing mark has fully landed and
// flipped marked, so the re-check here sees the truth (#60).
func (t *turnTracker) clear(sessionID string) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	skip := !t.marked[sessionID] || t.frozen || t.active[sessionID] > 0
	t.mu.Unlock()
	if skip {
		return
	}
	// Bounded: a wedged store write here would otherwise stall the
	// turn's return and burn the drain window (#48).
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := t.store.ClearInterrupted(ctx, defaultUserID, sessionID); err != nil {
		// Keep marked set (#64): the marker is still on disk, and
		// forgetting it here would hide it from survivors()
		// with nothing left to retry the clear.
		t.logger.Error("failed to clear interruption marker", "session", sessionID, "error", err.Error())
		return
	}
	t.mu.Lock()
	delete(t.marked, sessionID)
	t.mu.Unlock()
}

// isDraining reports whether a shutdown drain has begun. The attach
// RunTurn path consults it to refuse new work during termination.
func (t *turnTracker) isDraining() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.draining
}

// beginDrain flips the tracker into draining mode and durably marks
// every session with a turn currently in flight. Called once, from the
// shutdown goroutine, before waiting out the drain window. Each mark
// re-checks under writeMu that its session is still mid-turn, so a
// turn that finishes while the loop is writing is skipped rather than
// falsely recorded interrupted (#55/#60). ctx bounds the whole pass
// (the drain window) on top of the per-write cap (#63).
func (t *turnTracker) beginDrain(ctx context.Context) {
	t.mu.Lock()
	t.draining = true
	toMark := make([]string, 0, len(t.active))
	for sid := range t.active {
		toMark = append(toMark, sid)
	}
	t.mu.Unlock()
	sort.Strings(toMark)
	for _, sid := range toMark {
		if ctx.Err() != nil {
			t.logger.Error("drain deadline elapsed during pre-mark; remaining sessions unmarked", "unmarked_from", sid)
			return
		}
		t.mark(ctx, sid)
	}
}

// wait blocks until no turns are in flight or ctx is done, and returns
// the sessions still active (sorted; empty on a clean drain).
func (t *turnTracker) wait(ctx context.Context) []string {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if remaining := t.activeSessions(); len(remaining) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return t.activeSessions()
		case <-tick.C:
		}
	}
}

// freeze stops marker clearing permanently. Called when the drain
// window elapses, before the surviving turns' contexts are cancelled:
// their unwinding must leave the interruption markers in place.
func (t *turnTracker) freeze() {
	t.mu.Lock()
	t.frozen = true
	t.mu.Unlock()
}

func (t *turnTracker) activeSessions() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	sids := make([]string, 0, len(t.active))
	for sid := range t.active {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	return sids
}

// survivors splits the still-active sessions by whether their
// interruption marker durably landed — the honest inputs for the
// drain-expiry warning (#58/#63): a session that finished (and
// cleared) in the wait-to-freeze window appears in neither list, and
// a session whose mark write FAILED must not be reported as carrying
// a durable marker.
func (t *turnTracker) survivors() (marked, unmarked []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for sid := range t.active {
		if t.marked[sid] {
			marked = append(marked, sid)
		} else {
			unmarked = append(unmarked, sid)
		}
	}
	sort.Strings(marked)
	sort.Strings(unmarked)
	return marked, unmarked
}

// sessionTurnLocks serializes turns per session (#62): two concurrent
// runner turns on one session row are unsupported by ADK's
// optimistic concurrency — the second turn's first append kills it
// with a stale session error — so a same-session inject/resume waits
// for the in-flight turn instead of destroying one. The wait is a
// channel semaphore, not a sync.Mutex, so it honors the caller's
// context: a queued turn dies with its request/budget deadline, and
// cancelTurns at drain expiry reclaims waiters instead of leaving
// them pinned invisibly on a mutex (pre-merge gate finding on #66).
// Semaphores are never deleted; the map is bounded by the number of
// distinct sessions a daemon lifetime sees, same as meterPool.
type sessionTurnLocks struct {
	mu   sync.Mutex
	byID map[string]chan struct{}
}

func newSessionTurnLocks() *sessionTurnLocks {
	return &sessionTurnLocks{byID: map[string]chan struct{}{}}
}

// lock acquires sessionID's turn slot, waiting until it is free or
// ctx ends. On success it returns the release func; on context end it
// returns ctx's error and the caller must not proceed with the turn.
func (l *sessionTurnLocks) lock(ctx context.Context, sessionID string) (func(), error) {
	l.mu.Lock()
	sem, ok := l.byID[sessionID]
	if !ok {
		sem = make(chan struct{}, 1)
		l.byID[sessionID] = sem
	}
	l.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for session %q turn slot: %w", sessionID, ctx.Err())
	}
}
