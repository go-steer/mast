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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/genai"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"

	"github.com/go-steer/mast/pkg/envelope"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

func TestDrainBound(t *testing.T) {
	if got := drainBound(nil); got != defaultDrainBound {
		t.Errorf("drainBound(nil) = %v, want %v", got, defaultDrainBound)
	}
	b := &workload.Bundle{}
	if got := drainBound(b); got != defaultDrainBound {
		t.Errorf("drainBound(no budget) = %v, want %v", got, defaultDrainBound)
	}
	b.Budget.MaxWallclockSeconds = 120
	if got := drainBound(b); got != 120*time.Second {
		t.Errorf("drainBound(120s budget) = %v, want 2m", got)
	}
}

// trackerFixture returns a tracker over an in-memory session store
// with the given sessions pre-created (the daemon's runner creates
// sessions on first turn; tests create them up front).
func trackerFixture(t *testing.T, sessionIDs ...string) (*turnTracker, *transcript.Store) {
	t.Helper()
	svc := adksession.InMemoryService()
	for _, sid := range sessionIDs {
		if _, err := svc.Create(context.Background(), &adksession.CreateRequest{
			AppName:   appName,
			UserID:    defaultUserID,
			SessionID: sid,
		}); err != nil {
			t.Fatalf("create session %q: %v", sid, err)
		}
	}
	store := transcript.NewStore(svc, appName)
	return newTurnTracker(store, slog.Default(), observability.New(), "(test)"), store
}

func stateOf(t *testing.T, store *transcript.Store, sid string) string {
	t.Helper()
	d, err := store.Get(context.Background(), defaultUserID, sid)
	if err != nil {
		t.Fatalf("Get(%q): %v", sid, err)
	}
	return d.State
}

func TestTrackerPreMarksAndClearsOnCleanFinish(t *testing.T) {
	tr, store := trackerFixture(t, "s1", "s2")
	ctx := context.Background()

	tr.begin("s1")

	// Drain start pre-marks the in-flight session durably — and only it.
	tr.beginDrain(context.Background())
	if got := stateOf(t, store, "s1"); got != transcript.StateInterrupted {
		t.Errorf("s1 after beginDrain = %q, want interrupted", got)
	}
	if got := stateOf(t, store, "s2"); got != transcript.StateIdle {
		t.Errorf("s2 (no turn in flight) after beginDrain = %q, want idle", got)
	}

	// A clean finish inside the drain window clears the marker, and
	// wait reports a clean drain.
	tr.end("s1")
	if got := stateOf(t, store, "s1"); got != transcript.StateIdle {
		t.Errorf("s1 after clean finish = %q, want idle", got)
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if remaining := tr.wait(waitCtx); len(remaining) != 0 {
		t.Errorf("wait after clean finish = %v, want none", remaining)
	}
}

func TestTrackerFreezeKeepsMarkerOnCancelledTurn(t *testing.T) {
	tr, store := trackerFixture(t, "s1")
	tr.begin("s1")
	tr.beginDrain(context.Background())

	// The drain window elapsed: freeze, then the daemon cancels the
	// turn, whose unwinding calls end. The marker must survive.
	tr.freeze()
	tr.end("s1")
	if got := stateOf(t, store, "s1"); got != transcript.StateInterrupted {
		t.Errorf("s1 after freeze+end = %q, want interrupted (marker must survive)", got)
	}
}

func TestTrackerMarksTurnStartedMidDrain(t *testing.T) {
	tr, store := trackerFixture(t, "s1")
	tr.beginDrain(context.Background())

	// A turn that starts while draining is marked immediately...
	tr.begin("s1")
	if got := stateOf(t, store, "s1"); got != transcript.StateInterrupted {
		t.Errorf("s1 begun mid-drain = %q, want interrupted", got)
	}
	// ...and cleared like any other if it finishes in time.
	tr.end("s1")
	if got := stateOf(t, store, "s1"); got != transcript.StateIdle {
		t.Errorf("s1 finished mid-drain = %q, want idle", got)
	}
}

func TestTrackerWaitTimesOutWithSurvivors(t *testing.T) {
	tr, _ := trackerFixture(t, "s1", "s2")
	tr.begin("s1")
	tr.begin("s2")
	tr.begin("s2") // two concurrent turns on s2 count once

	tr.beginDrain(context.Background())
	waitCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	remaining := tr.wait(waitCtx)
	if len(remaining) != 2 || remaining[0] != "s1" || remaining[1] != "s2" {
		t.Errorf("wait survivors = %v, want [s1 s2]", remaining)
	}

	// One turn on s2 ending does not clear its marker (a second is
	// still in flight)...
	tr.end("s2")
	if got := tr.activeSessions(); len(got) != 2 {
		t.Errorf("activeSessions after one of two s2 turns ended = %v", got)
	}
}

// TestTrackerMarkingDoesNotKillLiveTurn closes the blind spot the
// adversarial review of #43 found: the original tests ran only against
// the in-memory service — the one implementation WITHOUT ADK's
// stale-session (write-lease) check — so they could not catch markers
// killing the turns they marked (#45). This runs the pre-mark path
// against a real SQLite database service while a simulated runner
// holds a live session handle.
func TestTrackerMarkingDoesNotKillLiveTurn(t *testing.T) {
	svc, err := database.NewSessionService(sqlite.Open(filepath.Join(t.TempDir(), "sessions.db")))
	if err != nil {
		t.Fatalf("open sqlite session service: %v", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: appName, UserID: defaultUserID, SessionID: "s-live",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The "runner": stream one event through a held handle. Natural
	// timestamps only — future-pinned stamps disarmed the OCC check
	// and let this test pass on the pre-fix code (#54).
	time.Sleep(2 * time.Millisecond)
	ev1 := adksession.NewEvent(ctx, "inv-live")
	ev1.Author = "triager"
	ev1.Content = genai.NewContentFromText("turn event 1", genai.RoleModel)
	if err := svc.AppendEvent(ctx, created.Session, ev1); err != nil {
		t.Fatalf("append pre-mark: %v", err)
	}

	store := transcript.NewStore(svc, appName)
	tr := newTurnTracker(store, slog.Default(), observability.New(), "(test)")
	tr.begin("s-live")
	tr.beginDrain(context.Background())

	// The marked turn keeps streaming — this append failed with
	// "stale session error" when markers wrote to the primary row.
	time.Sleep(2 * time.Millisecond)
	ev2 := adksession.NewEvent(ctx, "inv-live")
	ev2.Author = "triager"
	ev2.Content = genai.NewContentFromText("turn event 2", genai.RoleModel)
	if err := svc.AppendEvent(ctx, created.Session, ev2); err != nil {
		t.Fatalf("append after pre-mark: %v (write-lease regression)", err)
	}

	if got := stateOf(t, store, "s-live"); got != transcript.StateInterrupted {
		t.Errorf("state during drain = %q, want interrupted", got)
	}
	tr.end("s-live")
	if got := stateOf(t, store, "s-live"); got != transcript.StateIdle {
		t.Errorf("state after clean finish = %q, want idle", got)
	}
}

// TestDefaultSessionPathConcurrentWrites is #53's acceptance test:
// the plain --session-db path (no --attach-listen) must survive
// concurrent sessions writing at once — the raw, unhardened service
// lost transcript events AND drain-time interruption markers to
// immediate SQLITE_BUSY (lock-upgrade conflicts ignore busy_timeout;
// only write serialization prevents them). buildSessionService now
// routes through eventlog.OpenSessionService, which applies the same
// hardening as the attach path minus the overlay.
func TestDefaultSessionPathConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	svc, err := buildSessionService(ctx, "sqlite", filepath.Join(t.TempDir(), "sessions.db"), discardLogger())
	if err != nil {
		t.Fatalf("buildSessionService: %v", err)
	}

	const sessions = 6
	const eventsPerSession = 20
	sids := make([]string, sessions)
	handles := make([]adksession.Session, sessions)
	for i := range sids {
		sids[i] = fmt.Sprintf("s-conc-%d", i)
		resp, err := svc.Create(ctx, &adksession.CreateRequest{
			AppName: appName, UserID: defaultUserID, SessionID: sids[i],
		})
		if err != nil {
			t.Fatalf("create %s: %v", sids[i], err)
		}
		handles[i] = resp.Session
	}

	// Concurrent turns: each session streams events through its own
	// held handle (the runner's shape), all sessions at once.
	var wg sync.WaitGroup
	errs := make(chan error, sessions*eventsPerSession)
	for i := range sids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < eventsPerSession; j++ {
				ev := adksession.NewEvent(ctx, "inv-conc")
				ev.Author = "triager"
				ev.Content = genai.NewContentFromText(fmt.Sprintf("event %d", j), genai.RoleModel)
				if err := svc.AppendEvent(ctx, handles[i], ev); err != nil {
					errs <- fmt.Errorf("%s event %d: %w", sids[i], j, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent append failed: %v", err)
	}

	// Drain-time marking with every session mid-turn: all must be
	// durably marked (the unhardened path left up to 5/6 unmarked).
	store := transcript.NewStore(svc, appName)
	tr := newTurnTracker(store, slog.Default(), observability.New(), "(test)")
	for _, sid := range sids {
		tr.begin(sid)
	}
	tr.beginDrain(context.Background())
	for _, sid := range sids {
		if got := stateOf(t, store, sid); got != transcript.StateInterrupted {
			t.Errorf("%s after beginDrain = %q, want interrupted (marker lost)", sid, got)
		}
	}
}

// TestReservedOpsRowRefusedEverywhere pins #56: reserved IDs are not
// sessions on any surface.
func TestReservedOpsRowRefusedEverywhere(t *testing.T) {
	ctx := context.Background()
	tr, store := trackerFixture(t, "s1")
	_ = tr
	if err := store.MarkInterrupted(ctx, defaultUserID, "s1", "daemon shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}
	reserved := "s1:mast-ops"
	if !transcript.IsReservedSessionID(reserved) {
		t.Fatal("IsReservedSessionID(reserved) = false")
	}
	if _, err := store.Get(ctx, defaultUserID, reserved); err == nil {
		t.Error("Get(reserved) succeeded — phantom session")
	}
	if err := store.Abort(ctx, defaultUserID, reserved, "x"); err == nil {
		t.Error("Abort(reserved) succeeded")
	}
	if err := store.MarkInterrupted(ctx, defaultUserID, reserved, "x"); err == nil {
		t.Error("MarkInterrupted(reserved) succeeded — would nest ops rows")
	}
}

// TestTrackerMarkSkipsFinishedSession pins the #55 re-check: a mark
// decided while the turn was in flight but executed after it finished
// (the beginDrain-loop race window) must be skipped, not written —
// the old ordering could land the clear BEFORE the mark and leave a
// cleanly-finished session reporting interrupted.
func TestTrackerMarkSkipsFinishedSession(t *testing.T) {
	tr, store := trackerFixture(t, "s1")
	tr.mu.Lock()
	tr.draining = true
	tr.mu.Unlock()

	tr.begin("s1")
	tr.end("s1") // finished cleanly before the queued mark runs

	tr.mark(context.Background(), "s1") // the stale queued mark
	if got := stateOf(t, store, "s1"); got != transcript.StateIdle {
		t.Errorf("state after stale mark = %q, want idle (finished session must not be marked)", got)
	}
}

// TestTrackerNoFalseInterruptedOnConcurrentFinish is #60's reproducer,
// promoted per the standing lesson: race fixes are validated by
// re-running the discovering reproducer, not by unit tests of the new
// API. N sessions each finish their only turn concurrently with
// beginDrain; afterwards NO session may report interrupted. On the
// pre-#60 code (end() gating its clear on marked[] outside writeMu)
// this failed ~1/40 sessions per run with no fault injection.
func TestTrackerNoFalseInterruptedOnConcurrentFinish(t *testing.T) {
	ctx := context.Background()
	svc, err := buildSessionService(ctx, "sqlite", filepath.Join(t.TempDir(), "sessions.db"), discardLogger())
	if err != nil {
		t.Fatalf("buildSessionService: %v", err)
	}
	store := transcript.NewStore(svc, appName)

	const sessions = 40
	sids := make([]string, sessions)
	for i := range sids {
		sids[i] = fmt.Sprintf("s-finish-%d", i)
		if _, err := svc.Create(ctx, &adksession.CreateRequest{
			AppName: appName, UserID: defaultUserID, SessionID: sids[i],
		}); err != nil {
			t.Fatalf("create %s: %v", sids[i], err)
		}
	}

	tr := newTurnTracker(store, discardLogger(), observability.New(), "(test)")
	for _, sid := range sids {
		tr.begin(sid)
	}

	// All turns finish concurrently with the pre-mark pass.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tr.beginDrain(ctx)
	}()
	for _, sid := range sids {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			tr.end(sid)
		}(sid)
	}
	wg.Wait()

	for _, sid := range sids {
		if got := stateOf(t, store, sid); got == transcript.StateInterrupted {
			t.Errorf("%s finished cleanly but reports interrupted (mark/clear ordering regression, #60)", sid)
		}
	}
	if marked, unmarked := tr.survivors(); len(marked)+len(unmarked) != 0 {
		t.Errorf("survivors after all turns ended = %v + %v, want none", marked, unmarked)
	}
}

// TestSessionTurnLocksPreventSameSessionCollision pins #62. The first
// subtest documents WHY the lock exists: two interleaved handles on
// one session row collide deterministically on ADK's stale-session
// check. The second proves serialized same-session turns all land.
func TestSessionTurnLocksPreventSameSessionCollision(t *testing.T) {
	ctx := context.Background()
	svc, err := buildSessionService(ctx, "sqlite", filepath.Join(t.TempDir(), "sessions.db"), discardLogger())
	if err != nil {
		t.Fatalf("buildSessionService: %v", err)
	}
	if _, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: appName, UserID: defaultUserID, SessionID: "s-same",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	appendOne := func(h adksession.Session, text string) error {
		ev := adksession.NewEvent(ctx, "inv-same")
		ev.Author = "triager"
		ev.Content = genai.NewContentFromText(text, genai.RoleModel)
		return svc.AppendEvent(ctx, h, ev)
	}
	get := func() adksession.Session {
		resp, err := svc.Get(ctx, &adksession.GetRequest{
			AppName: appName, UserID: defaultUserID, SessionID: "s-same",
		})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return resp.Session
	}

	t.Run("unserialized handles collide", func(t *testing.T) {
		a, b := get(), get()
		time.Sleep(2 * time.Millisecond)
		if err := appendOne(a, "turn A event"); err != nil {
			t.Fatalf("append via A: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
		if err := appendOne(b, "turn B event"); err == nil {
			t.Fatal("append via stale handle B succeeded — ADK dropped its OCC check? the turn lock may be removable")
		}
	})

	t.Run("serialized turns all land", func(t *testing.T) {
		locks := newSessionTurnLocks()
		var wg sync.WaitGroup
		errs := make(chan error, 2*10)
		for turn := 0; turn < 2; turn++ {
			wg.Add(1)
			go func(turn int) {
				defer wg.Done()
				unlock, err := locks.lock(ctx, "s-same") // the runTurn discipline
				if err != nil {
					errs <- err
					return
				}
				defer unlock()
				h := get() // fresh handle per turn, like the runner
				for j := 0; j < 10; j++ {
					time.Sleep(time.Millisecond)
					if err := appendOne(h, fmt.Sprintf("turn %d event %d", turn, j)); err != nil {
						errs <- err
						return
					}
				}
			}(turn)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("serialized same-session append failed: %v", err)
		}
	})
}

// TestReservedPayloadErr pins the #61 inject-surface guard: a payload
// UID deriving a reserved ops-row session ID is rejected with the
// 400-mapped sentinel; ordinary UIDs pass.
func TestReservedPayloadErr(t *testing.T) {
	bad := envelope.InjectPayload{UID: "x:mast-ops"}
	err := reservedPayloadErr(bad)
	if err == nil {
		t.Fatal("reserved UID accepted")
	}
	if !errors.Is(err, inject.ErrBadPayload) {
		t.Errorf("reserved UID error = %v, want wrapped inject.ErrBadPayload (400, not 500)", err)
	}
	if err := reservedPayloadErr(envelope.InjectPayload{UID: "ordinary-uid"}); err != nil {
		t.Errorf("ordinary UID rejected: %v", err)
	}
	// Empty UID falls back to defaultSessionID, which must never be
	// reserved.
	if err := reservedPayloadErr(envelope.InjectPayload{}); err != nil {
		t.Errorf("empty UID rejected: %v", err)
	}
}

// TestSessionTurnLockHonorsContext pins the semaphore contract the
// pre-merge gate refuted for the sync.Mutex version: a queued waiter
// must abandon the wait when its context ends, so drain-expiry
// cancellation reclaims queued turns instead of leaving them pinned.
func TestSessionTurnLockHonorsContext(t *testing.T) {
	locks := newSessionTurnLocks()
	unlock, err := locks.lock(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() {
		_, err := locks.lock(ctx, "s1")
		waited <- err
	}()
	cancel()
	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued waiter ignored context cancellation")
	}

	unlock()
	// The slot is reusable after release.
	unlock2, err := locks.lock(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	unlock2()
}

// TestTeardownWatchdogFiresOnOverrun: when teardown outlives the
// deadline, the watchdog dumps goroutine stacks and exits with the
// dedicated hang code. Both effects are injected so the test observes
// them without terminating.
func TestTeardownWatchdogFiresOnOverrun(t *testing.T) {
	var dumped bytes.Buffer
	dumpDone := make(chan struct{})
	dump := func(w io.Writer) {
		dumpGoroutines(&dumped) // exercise the real dump into our buffer
		close(dumpDone)
	}
	gotExit := make(chan int, 1)
	exit := func(code int) { gotExit <- code }

	armTeardownWatchdog(time.Millisecond, dump, exit, discardLogger())

	select {
	case code := <-gotExit:
		if code != teardownHangExitCode {
			t.Fatalf("exit code = %d, want %d", code, teardownHangExitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire within 2s")
	}
	<-dumpDone
	if !strings.Contains(dumped.String(), "goroutine") {
		t.Errorf("goroutine dump missing stack content:\n%s", dumped.String())
	}
}

// TestTeardownWatchdogQuietBeforeDeadline: a watchdog with a long
// deadline must not fire (dump/exit) before it — a healthy teardown
// reaches os.Exit first and kills the sleeping goroutine.
func TestTeardownWatchdogQuietBeforeDeadline(t *testing.T) {
	fired := make(chan struct{}, 1)
	dump := func(io.Writer) { fired <- struct{}{} }
	exit := func(int) { fired <- struct{}{} }

	armTeardownWatchdog(time.Hour, dump, exit, discardLogger())

	select {
	case <-fired:
		t.Fatal("watchdog fired before its deadline")
	case <-time.After(50 * time.Millisecond):
		// expected: still parked
	}
}
