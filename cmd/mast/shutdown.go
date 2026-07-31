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
		t.mark(sessionID)
	}
}

// end records a turn finishing on sessionID, clearing the session's
// interruption marker when this was its last in-flight turn and the
// tracker is not frozen.
func (t *turnTracker) end(sessionID string) {
	t.mu.Lock()
	t.active[sessionID]--
	if t.active[sessionID] <= 0 {
		delete(t.active, sessionID)
	}
	maybeClear := t.marked[sessionID] && !t.frozen && t.active[sessionID] == 0
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
func (t *turnTracker) mark(sessionID string) {
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
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
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
// same writeMu so it is strictly ordered after any in-flight mark.
func (t *turnTracker) clear(sessionID string) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	skip := !t.marked[sessionID] || t.frozen || t.active[sessionID] > 0
	if !skip {
		delete(t.marked, sessionID)
	}
	t.mu.Unlock()
	if skip {
		return
	}
	// Bounded: a wedged store write here would otherwise stall the
	// turn's return and burn the drain window (#48).
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := t.store.ClearInterrupted(ctx, defaultUserID, sessionID); err != nil {
		t.logger.Error("failed to clear interruption marker", "session", sessionID, "error", err.Error())
	}
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
// falsely recorded interrupted (#55).
func (t *turnTracker) beginDrain() {
	t.mu.Lock()
	t.draining = true
	toMark := make([]string, 0, len(t.active))
	for sid := range t.active {
		toMark = append(toMark, sid)
	}
	t.mu.Unlock()
	sort.Strings(toMark)
	for _, sid := range toMark {
		t.mark(sid)
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

// markedActiveSessions returns the sessions that are BOTH still
// mid-turn and durably marked — the honest survivor list for the
// drain-expiry warning (#58): a session that finished (and cleared)
// in the wait-to-freeze window must not be reported interrupted.
func (t *turnTracker) markedActiveSessions() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	sids := make([]string, 0, len(t.active))
	for sid := range t.active {
		if t.marked[sid] {
			sids = append(sids, sid)
		}
	}
	sort.Strings(sids)
	return sids
}
