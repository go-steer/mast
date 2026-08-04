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

// Tests for the v0.2 durable-execution metric emission sites (issue
// #50): every counter fires only when its underlying durable operation
// actually succeeded (or, for the marker-failure family, only when it
// failed). Each assertion is neutralize-verifiable — deleting the
// emission at the site under test drops the scraped value and fails
// here.
package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
)

// primedObs returns a registry with the "(test)" workload's families
// materialized at zero, so an assertMetric for a "... 0" line finds the
// series present (Prometheus omits series never touched).
func primedObs() *observability.Registry {
	r := observability.New()
	r.Prime("(test)")
	return r
}

// assertMetric scrapes obs and fails unless the exact Prometheus sample
// line is present. Black-box on purpose: it exercises the same /metrics
// surface an operator scrapes, so a label rename or a dropped emission
// both show up here.
func assertMetric(t *testing.T, obs *observability.Registry, sample string) {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), sample) {
		t.Errorf("scrape missing %q\nscrape:\n%s", sample, rec.Body.String())
	}
}

// failingService wraps a real session service and injects AppendEvent
// failures, selected by the event's invocation ID so a test can fail
// one marker write (mark, or clear) while letting the others land.
type failingService struct {
	adksession.Service
	failInvocation string // fail AppendEvent when ev.InvocationID == this
}

func (f *failingService) AppendEvent(ctx context.Context, sess adksession.Session, ev *adksession.Event) error {
	if f.failInvocation != "" && ev.InvocationID == f.failInvocation {
		return errors.New("injected append failure")
	}
	return f.Service.AppendEvent(ctx, sess, ev)
}

// TestRecordAbortCountsOnlyOnSuccess: mast_aborts_total advances when
// the abort marker lands, and NOT when the write is refused.
func TestRecordAbortCountsOnlyOnSuccess(t *testing.T) {
	ctx := context.Background()
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-abort-metric")
	store := transcript.NewStore(svc, appName)
	obs := primedObs()

	if err := recordAbort(ctx, store, obs, "(test)", "s-abort-metric", "operator cancelled"); err != nil {
		t.Fatalf("recordAbort: %v", err)
	}
	assertMetric(t, obs, `mast_aborts_total{workload="(test)"} 1`)

	// A refused abort (reserved ops-row ID) must not advance the counter.
	if err := recordAbort(ctx, store, obs, "(test)", "s-abort-metric:mast-ops", "x"); err == nil {
		t.Fatal("recordAbort on a reserved ID succeeded; expected refusal")
	}
	assertMetric(t, obs, `mast_aborts_total{workload="(test)"} 1`)
}

// TestOpenGatePauseCountsOperatorOnlyOnSuccess: the operator-sourced
// gate-pause counter advances on a durable pause and stays put when
// PauseGate refuses (here: the session is already aborted).
func TestOpenGatePauseCountsOperatorOnlyOnSuccess(t *testing.T) {
	ctx := context.Background()
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-gp-metric")
	store := transcript.NewStore(svc, appName)
	obs := primedObs()

	if _, err := openGatePause(ctx, store, obs, "(test)", "s-gp-metric", transcript.PauseSpec{
		Reason: transcript.ReasonMaintenanceWindow, Message: "deploy window",
	}); err != nil {
		t.Fatalf("openGatePause: %v", err)
	}
	assertMetric(t, obs, `mast_gate_pauses_total{source="operator",workload="(test)"} 1`)

	// A second /pause on the still-active session refreshes it in place
	// (same token, no new pause), so the operator counter must stay at 1
	// (#50 regression: openGatePause used to count every nil return).
	if _, err := openGatePause(ctx, store, obs, "(test)", "s-gp-metric", transcript.PauseSpec{
		Reason: transcript.ReasonOperator, Message: "adjust window",
	}); err != nil {
		t.Fatalf("openGatePause refresh: %v", err)
	}
	assertMetric(t, obs, `mast_gate_pauses_total{source="operator",workload="(test)"} 1`)

	// Abort the session, then a pause must be refused and not counted.
	if err := store.Abort(ctx, "", "s-gp-metric", "terminal"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := openGatePause(ctx, store, obs, "(test)", "s-gp-metric", transcript.PauseSpec{
		Reason: transcript.ReasonMaintenanceWindow,
	}); err == nil {
		t.Fatal("openGatePause on an aborted session succeeded; expected refusal")
	}
	assertMetric(t, obs, `mast_gate_pauses_total{source="operator",workload="(test)"} 1`)
}

// TestTimedFireCallbackEmitsOutcome drives the real daemon fire
// callback (not a test twin) through the scheduler: a due gate pause
// fires as resumed; a fire while the daemon is draining is skipped.
func TestTimedFireCallbackEmitsOutcome(t *testing.T) {
	ctx := context.Background()
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-tf-ok")
	seedSession(t, svc, "s-tf-drain")
	store := transcript.NewStore(svc, appName)
	obs := primedObs()
	tr := newTurnTracker(store, discardLogger(), obs, "(test)")
	cb := newTimedFireCallback(store, tr, obs, "(test)", nil, discardLogger())
	sched := newPauseScheduler(store, discardLogger(), cb)

	okHandle, _, err := store.PauseGate(ctx, "", "s-tf-ok", transcript.PauseSpec{
		Reason:   transcript.ReasonRateLimitBackoff,
		ResumeAt: time.Now().UTC().Add(-time.Second), // already due
	})
	if err != nil {
		t.Fatalf("PauseGate ok: %v", err)
	}
	sched.fireDue(ctx, okHandle.Token)
	assertMetric(t, obs, `mast_timed_pause_fires_total{outcome="resumed",workload="(test)"} 1`)

	// A second due token, but the tracker is now draining: the fire is a
	// skip (and requeues, but that is the scheduler's business).
	drainHandle, _, err := store.PauseGate(ctx, "", "s-tf-drain", transcript.PauseSpec{
		Reason:   transcript.ReasonRateLimitBackoff,
		ResumeAt: time.Now().UTC().Add(-time.Second),
	})
	if err != nil {
		t.Fatalf("PauseGate drain: %v", err)
	}
	tr.mu.Lock()
	tr.draining = true
	tr.mu.Unlock()
	sched.fireDue(ctx, drainHandle.Token)
	assertMetric(t, obs, `mast_timed_pause_fires_total{outcome="skipped",workload="(test)"} 1`)
}

// TestTimedFireCallbackInterruptPlane drives the interrupt-plane branch
// of the real fire callback (TestTimedFireCallbackEmitsOutcome covers
// the gate plane): a due interrupt pause whose resume lands is
// "resumed"; one whose resume errors is "error". Both route through
// resumeByInterrupt — the same daemon door an operator resume uses.
func TestTimedFireCallbackInterruptPlane(t *testing.T) {
	ctx := context.Background()
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-tf-intr-ok")
	seedSession(t, svc, "s-tf-intr-err")
	store := transcript.NewStore(svc, appName)
	obs := primedObs()
	tr := newTurnTracker(store, discardLogger(), obs, "(test)")

	// A toggleable resume result: nil for the resumed leg, an error for
	// the error leg. The callback classifies the outcome off this return.
	var resumeErr error
	resumeByInterrupt := func(context.Context, inject.ResumeRequest) error { return resumeErr }
	cb := newTimedFireCallback(store, tr, obs, "(test)", resumeByInterrupt, discardLogger())
	sched := newPauseScheduler(store, discardLogger(), cb)

	okHandle, err := store.PauseInterrupt(ctx, "", "s-tf-intr-ok", "intr-ok", transcript.PauseSpec{
		Reason:   transcript.ReasonRateLimitBackoff,
		ResumeAt: time.Now().UTC().Add(-time.Second), // already due
	})
	if err != nil {
		t.Fatalf("PauseInterrupt ok: %v", err)
	}
	resumeErr = nil
	sched.fireDue(ctx, okHandle.Token)
	assertMetric(t, obs, `mast_timed_pause_fires_total{outcome="resumed",workload="(test)"} 1`)

	errHandle, err := store.PauseInterrupt(ctx, "", "s-tf-intr-err", "intr-err", transcript.PauseSpec{
		Reason:   transcript.ReasonRateLimitBackoff,
		ResumeAt: time.Now().UTC().Add(-time.Second),
	})
	if err != nil {
		t.Fatalf("PauseInterrupt err: %v", err)
	}
	resumeErr = errors.New("resume failed")
	sched.fireDue(ctx, errHandle.Token)
	assertMetric(t, obs, `mast_timed_pause_fires_total{outcome="error",workload="(test)"} 1`)
}

// TestMarkerWriteFailureCounter pins the item-1 gap #50 closes: a lost
// interruption-marker write is silent without a counter. A mark that
// fails advances {operation=mark}; a clear that fails (after a mark
// that landed) advances {operation=clear}.
func TestMarkerWriteFailureCounter(t *testing.T) {
	t.Run("mark failure", func(t *testing.T) {
		svc := &failingService{Service: adksession.InMemoryService(), failInvocation: "shutdown-interrupt"}
		store := transcript.NewStore(svc, appName)
		obs := primedObs()
		tr := newTurnTracker(store, discardLogger(), obs, "(test)")

		tr.begin("s-markfail")
		tr.beginDrain(context.Background()) // marks the in-flight session; the write fails

		assertMetric(t, obs, `mast_marker_write_failures_total{operation="mark",workload="(test)"} 1`)
	})

	t.Run("clear failure", func(t *testing.T) {
		svc := &failingService{Service: adksession.InMemoryService(), failInvocation: "shutdown-interrupt-clear"}
		store := transcript.NewStore(svc, appName)
		obs := primedObs()
		tr := newTurnTracker(store, discardLogger(), obs, "(test)")

		tr.begin("s-clearfail")
		tr.beginDrain(context.Background()) // mark lands (its invocation is not failed)
		tr.end("s-clearfail")               // clean finish → clear, whose write fails

		assertMetric(t, obs, `mast_marker_write_failures_total{operation="clear",workload="(test)"} 1`)
		assertMetric(t, obs, `mast_marker_write_failures_total{operation="mark",workload="(test)"} 0`)
	})

	// pause failure exercises the planned-stop --pause-sessions path: the
	// interruption mark lands but the gate-pause write (ops invocation
	// "pause") is refused. This is the one emission site that would
	// otherwise survive deletion with all tests green.
	t.Run("pause failure", func(t *testing.T) {
		svc := &failingService{Service: adksession.InMemoryService(), failInvocation: "pause"}
		seedSession(t, svc, "s-pausefail") // PauseGate needs the session to exist
		store := transcript.NewStore(svc, appName)
		obs := primedObs()
		tr := newTurnTracker(store, discardLogger(), obs, "(test)")

		tr.begin("s-pausefail")
		tr.planStop("planned stop test", true) // --pause-sessions: mark THEN pause
		tr.beginDrain(context.Background())    // mark lands; the pause write fails

		assertMetric(t, obs, `mast_marker_write_failures_total{operation="pause",workload="(test)"} 1`)
		assertMetric(t, obs, `mast_marker_write_failures_total{operation="mark",workload="(test)"} 0`)
		// The failed pause must NOT be counted as a durable planned-stop pause.
		assertMetric(t, obs, `mast_gate_pauses_total{source="planned_stop",workload="(test)"} 0`)
	})
}
