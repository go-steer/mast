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

// The scheduler's boot scan covers what predates the process, and the
// push covers what this process minted. #345 adds a third case that
// neither covers: a timed pause POSTed to a replica that does not hold
// the scheduling lease. That record is durable and correct, and before
// the rescan nothing armed it until the leader restarted.
package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/transcript"
)

// listFailingService is a session service whose List can be switched
// off and back on, which is how ScanPauses is made to fail: the rescan
// reads through List.
type listFailingService struct {
	adksession.Service
	down atomic.Bool
}

func (s *listFailingService) List(ctx context.Context, req *adksession.ListRequest) (*adksession.ListResponse, error) {
	if s.down.Load() {
		return nil, errors.New("session store is unreachable")
	}
	return s.Service.List(ctx, req)
}

// The headline: a pause record the scheduler has never heard of gets
// armed and fired by the rescan alone — no push, no boot scan, no
// restart.
func TestRescanFiresATimerMintedOnAnotherInstance(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-rescan")
	store := transcript.NewStore(svc, appName)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := make(chan string, 4)
	sched := newPauseScheduler(store, discardLogger(), func(_ context.Context, rec *transcript.PauseRecord) error {
		fired <- rec.Token
		_, err := store.ConsumeToken(ctx, rec.Token, "timer")
		return err
	})
	sched.rescanEvery = 5 * time.Millisecond

	// The scheduler is running, and it has already done its boot scan
	// over an empty store — this is a live leader, not one starting up.
	if err := sched.seed(ctx); err != nil {
		t.Fatalf("boot scan: %v", err)
	}
	go sched.run(ctx)
	go sched.runRescan(ctx)

	// Now the other replica takes the operator's request. It writes the
	// record; this process is never told.
	handle, _, err := store.PauseGate(ctx, "", "s-rescan", transcript.PauseSpec{
		Reason:   transcript.ReasonRateLimitBackoff,
		ResumeAt: time.Now().UTC().Add(-time.Second), // already due
	})
	if err != nil {
		t.Fatalf("PauseGate on the other instance: %v", err)
	}

	select {
	case tok := <-fired:
		if tok != handle.Token {
			t.Fatalf("fired %q, want the token minted elsewhere %q", tok, handle.Token)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a timed pause minted on another instance never fired; the rescan did not pick it up")
	}

	// And exactly once: the rescan keeps ticking, so a consumed token
	// must not come back round. fireDue re-fetches, which is what makes
	// that true — this asserts it, rather than trusting it.
	select {
	case tok := <-fired:
		t.Errorf("the rescan re-fired a consumed token %q", tok)
	case <-time.After(200 * time.Millisecond):
	}
}

// Re-arming a token that is still pending must move its time, not queue
// a second fire — the property that lets the rescan run on a cadence
// next to the push without the two fighting.
func TestRescanIsIdempotentAgainstAPendingTimer(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-rescan-idem")
	store := transcript.NewStore(svc, appName)
	ctx := context.Background()

	sched := newPauseScheduler(store, discardLogger(), func(context.Context, *transcript.PauseRecord) error {
		return nil
	})
	handle, _, err := store.PauseGate(ctx, "", "s-rescan-idem", transcript.PauseSpec{
		Reason:   transcript.ReasonMaintenanceWindow,
		ResumeAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}

	for i := range 3 {
		n, err := sched.scan(ctx)
		if err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("scan %d armed %d timers, want the 1 active record", i, n)
		}
	}
	sched.mu.Lock()
	entries := len(sched.entries)
	_, present := sched.entries[handle.Token]
	sched.mu.Unlock()
	if entries != 1 || !present {
		t.Errorf("three scans left %d entries (token present: %v), want exactly 1", entries, present)
	}
}

// A store that is down must not produce a log line every minute for as
// long as it stays down — and must say when it comes back, because
// "scheduled work is broken" and "scheduled work was broken" are
// different pages.
func TestRescanLogsOnTransitionNotOnEveryTick(t *testing.T) {
	inner := adksession.InMemoryService()
	seedSession(t, inner, "s-rescan-log")
	svc := &listFailingService{Service: inner}
	store := transcript.NewStore(svc, appName)

	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sched := newPauseScheduler(store, logger, func(context.Context, *transcript.PauseRecord) error {
		return nil
	})
	sched.rescanEvery = 2 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.down.Store(true)
	go sched.runRescan(ctx)

	waitFor(t, buf, "rescan failed")

	// Many ticks have passed by now; the failure must still be one line.
	time.Sleep(100 * time.Millisecond)
	if n := strings.Count(buf.String(), "rescan failed"); n != 1 {
		t.Errorf("a store that stayed down logged %d times; want 1 line per transition", n)
	}

	svc.down.Store(false)
	waitFor(t, buf, "rescan recovered")
	time.Sleep(100 * time.Millisecond)
	if n := strings.Count(buf.String(), "rescan recovered"); n != 1 {
		t.Errorf("recovery logged %d times; want 1 line per transition", n)
	}
}

func TestRescanStopsWithItsContext(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-rescan-stop")
	store := transcript.NewStore(svc, appName)
	sched := newPauseScheduler(store, discardLogger(), func(context.Context, *transcript.PauseRecord) error {
		return nil
	})
	sched.rescanEvery = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sched.runRescan(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runRescan outlived its context; losing the lease would not stop it")
	}
}
