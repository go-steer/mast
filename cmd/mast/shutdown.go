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

// drainBound returns how long a shutdown waits for in-flight turns.
// Serve-mode turns are already wallclock-bounded by the workload
// budget, so the drain bound IS that ceiling — a finishing turn is
// never cut shorter than its own budget allows, and the worst-case
// drain is known at deploy time (size terminationGracePeriodSeconds /
// TimeoutStopSec above it).
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
// The zero tracker is not usable; construct via newTurnTracker.
type turnTracker struct {
	store  *transcript.Store
	userID string
	logger *slog.Logger

	mu       sync.Mutex
	active   map[string]int  // sessionID → in-flight turn count
	marked   map[string]bool // sessions carrying an uncleared marker
	draining bool
	frozen   bool
}

func newTurnTracker(store *transcript.Store, userID string, logger *slog.Logger) *turnTracker {
	return &turnTracker{
		store:  store,
		userID: userID,
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
	if mark {
		t.marked[sessionID] = true
	}
	t.mu.Unlock()
	if mark {
		t.writeMark(context.Background(), sessionID)
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
	clear := t.marked[sessionID] && !t.frozen && t.active[sessionID] == 0
	if clear {
		delete(t.marked, sessionID)
	}
	t.mu.Unlock()
	if clear {
		if err := t.store.ClearInterrupted(context.Background(), t.userID, sessionID); err != nil {
			t.logger.Warn("failed to clear interruption marker", "session", sessionID, "error", err.Error())
		}
	}
}

// beginDrain flips the tracker into draining mode and durably marks
// every session with a turn currently in flight. Called once, from the
// shutdown goroutine, before waiting out the drain window.
func (t *turnTracker) beginDrain(ctx context.Context) {
	t.mu.Lock()
	t.draining = true
	var toMark []string
	for sid := range t.active {
		if !t.marked[sid] {
			t.marked[sid] = true
			toMark = append(toMark, sid)
		}
	}
	t.mu.Unlock()
	sort.Strings(toMark)
	for _, sid := range toMark {
		t.writeMark(ctx, sid)
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

func (t *turnTracker) writeMark(ctx context.Context, sessionID string) {
	if err := t.store.MarkInterrupted(ctx, t.userID, sessionID, "daemon shutdown"); err != nil {
		t.logger.Warn("failed to write interruption marker", "session", sessionID, "error", err.Error())
	}
}
