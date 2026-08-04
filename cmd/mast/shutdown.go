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
	"io"
	"log/slog"
	"os"
	"runtime/pprof"
	"sort"
	"sync"
	"time"

	"github.com/go-steer/mast/pkg/observability"
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

// teardownWatchdogTimeout bounds serve()'s post-drain teardown — the
// deferred OTel flush, eventlog Close, attach-server Close, and context
// cancellations that run while serve() unwinds. The drain itself has
// already completed by the time the watchdog arms (it is armed after
// <-shutdownDone), so this covers only the Close/flush path, which is
// sub-second in the healthy case. 15s is generous headroom; overrunning
// it means a Close deadlocked or a goroutine will not die, which is a
// bug we want surfaced, not a hang the supervisor eventually SIGKILLs
// with no diagnostic.
const teardownWatchdogTimeout = 15 * time.Second

// teardownHangExitCode is the process exit status when the teardown
// watchdog fires. It is distinct from the drain-expired code (3): a
// drain that expires with survivors is an expected, recoverable outcome
// (the boot pass revives the work), whereas a teardown that overruns is
// a latent bug — an unkillable goroutine or a wedged Close — and an
// operator's response differs. 0/1/2/3 are taken (clean/error/flag/
// drain-expired); this is the next free code.
const teardownHangExitCode = 4

// armTeardownWatchdog starts a detached goroutine that, if serve()'s
// teardown has not finished within d, dumps every goroutine's stack (so
// the wedged Close or leaked goroutine is named in the logs) and forces
// the process to exit. No disarm is needed: a healthy teardown returns
// from serve(), run() calls os.Exit with the real status, and that kills
// this sleeping goroutine before its timer elapses. The dump/exit
// functions are injected so the fire path is unit-testable without
// actually terminating the test process.
func armTeardownWatchdog(d time.Duration, dump func(io.Writer), exit func(int), logger *slog.Logger) {
	go func() {
		time.Sleep(d)
		logger.Error("teardown exceeded deadline; dumping goroutine stacks and force-exiting",
			"deadline", d.String(), "exit_code", teardownHangExitCode)
		dump(os.Stderr)
		exit(teardownHangExitCode)
	}()
}

// dumpGoroutines writes every goroutine's stack (debug level 2) to w.
// It is the default dump function for armTeardownWatchdog.
func dumpGoroutines(w io.Writer) {
	_ = pprof.Lookup("goroutine").WriteTo(w, 2)
}

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
	store    *transcript.Store
	logger   *slog.Logger
	obs      *observability.Registry
	workload string // metric label; resolved once at construction

	mu       sync.Mutex
	active   map[string]int  // sessionID → in-flight turn count
	marked   map[string]bool // sessions with a durably-written, uncleared marker
	draining bool
	frozen   bool

	// cancels holds each in-flight turn's cancel handle (one turn per
	// session, #62). The v0.2 pause/abort handshake: the turn REGISTERS
	// under the session turn lock, before the chokepoint's marker
	// check; abort / hard pause writes its marker, then SWEEPS via
	// cancelSession. A turn is therefore always either registered (the
	// sweep cancels it) or not yet past the chokepoint check (which
	// sees the marker and refuses) — no unseen window.
	cancels map[string]context.CancelFunc

	// stopReason classifies the drain's interruption markers: the
	// signal path's "daemon shutdown", or "operator stop[: reason]"
	// when a planned stop (POST /stop) initiated it (issue #42).
	stopReason string

	// pauseOnDrain gate-pauses every session as it is marked (the
	// planned stop's --pause-sessions): sited inside mark(), under
	// writeMu, so turns that start mid-drain and get marked late are
	// paused too — a one-shot sweep before draining would miss them.
	pauseOnDrain bool

	writeMu sync.Mutex // serializes mark/clear decision + store write
}

func newTurnTracker(store *transcript.Store, logger *slog.Logger, obs *observability.Registry, workload string) *turnTracker {
	return &turnTracker{
		store:      store,
		logger:     logger,
		obs:        obs,
		workload:   workload,
		active:     map[string]int{},
		marked:     map[string]bool{},
		cancels:    map[string]context.CancelFunc{},
		stopReason: "daemon shutdown",
	}
}

// registerCancel records the in-flight turn's cancel handle. Called
// under the session turn lock, BEFORE the chokepoint's marker check
// (the register-before-check half of the abort/pause handshake).
func (t *turnTracker) registerCancel(sessionID string, cancel context.CancelFunc) {
	t.mu.Lock()
	t.cancels[sessionID] = cancel
	t.mu.Unlock()
}

// unregisterCancel drops the handle when the turn ends.
func (t *turnTracker) unregisterCancel(sessionID string) {
	t.mu.Lock()
	delete(t.cancels, sessionID)
	t.mu.Unlock()
}

// cancelSession cancels the session's in-flight turn, if any — the
// mark-then-sweep half of the handshake (terminal abort, and gate
// pause with Interrupt: true). Callers write their durable marker
// FIRST: the cancellation itself leaves no engine record (verified —
// ADK maps context.Canceled node completions to a silent drain), so
// the marker is the only durable truth.
func (t *turnTracker) cancelSession(sessionID string) bool {
	t.mu.Lock()
	cancel, ok := t.cancels[sessionID]
	t.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// planStop classifies the coming drain as operator-initiated (issue
// #42): interruption markers carry reason instead of "daemon
// shutdown", and pauseSessions arms the pause-and-mark pass.
func (t *turnTracker) planStop(reason string, pauseSessions bool) {
	t.mu.Lock()
	t.stopReason = reason
	t.pauseOnDrain = pauseSessions
	t.mu.Unlock()
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
	reason := t.stopReason
	pause := t.pauseOnDrain
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
	if err := t.store.MarkInterrupted(ctx, defaultUserID, sessionID, reason); err != nil {
		// Error, not Warn (#53): a lost marker means an interrupted
		// turn a restart will never surface. The counter (#50) makes
		// that alertable — a lost marker is silent otherwise.
		t.obs.MarkerWriteFailure(t.workload, observability.MarkerOpMark)
		t.logger.Error("failed to write interruption marker", "session", sessionID, "error", err.Error())
		return
	}
	if pause {
		// Planned stop with --pause-sessions: gate-pause under the same
		// writeMu hold, so the pause travels with the mark (adversarial
		// gate finding L1 — late-starting turns get both or neither).
		// Paused outranks interrupted in the state ladder, so boot-time
		// auto-resume (#41, candidates = interrupted) skips the session.
		_, created, err := t.store.PauseGate(ctx, defaultUserID, sessionID, transcript.PauseSpec{
			Reason:  transcript.ReasonOperator,
			Message: "planned stop: " + reason,
		})
		switch {
		case err != nil:
			t.obs.MarkerWriteFailure(t.workload, observability.MarkerOpPause)
			t.logger.Error("failed to gate-pause session for planned stop", "session", sessionID, "error", err.Error())
		case created:
			// Count only a newly-opened pause; a session already
			// gate-paused before the stop is refreshed in place, not
			// newly gated (#50 — mirrors openGatePause's operator count).
			t.obs.GatePause(t.workload, observability.GatePausePlannedStop)
		}
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
		t.obs.MarkerWriteFailure(t.workload, observability.MarkerOpClear)
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
