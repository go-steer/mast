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
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"

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
	return newTurnTracker(store, defaultUserID, slog.Default()), store
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
	tr.beginDrain(ctx)
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
