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

// Tests for the boot-time auto-resume pass (issue #41): the eligibility
// gate (dangling mutating → ambiguous, delegation → unsupported), the
// trailing-event classification (repair / re-run / clear), and the
// freshness, restart-loop, and supersession rails.
package main

import (
	"context"
	"testing"
	"time"

	"google.golang.org/genai"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
)

// --- seeding helpers ---------------------------------------------------

func userText(text string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-user")
	ev.Author = "user"
	ev.Content = genai.NewContentFromText(text, genai.RoleUser)
	return ev
}

func modelText(text string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-model")
	ev.Author = "pause_abort_agent"
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	return ev
}

// modelCall is a model turn emitting one FunctionCall with no response —
// a call left dangling by an interruption.
func modelCall(id, tool string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-"+id)
	ev.Author = "pause_abort_agent"
	ev.Content = &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: tool, Args: map[string]any{}}}},
	}
	return ev
}

// seed creates a primary session under the daemon's user id and appends
// the events in order, re-getting the handle each time (the in-memory
// service validates the handle on append).
func (h *turnHarness) seed(t *testing.T, sid string, events ...*adksession.Event) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.svc.Create(ctx, &adksession.CreateRequest{
		AppName: appName, UserID: defaultUserID, SessionID: sid,
	}); err != nil {
		t.Fatalf("create %q: %v", sid, err)
	}
	for i, ev := range events {
		resp, err := h.svc.Get(ctx, &adksession.GetRequest{
			AppName: appName, UserID: defaultUserID, SessionID: sid,
		})
		if err != nil {
			t.Fatalf("get %q for append %d: %v", sid, i, err)
		}
		if err := h.svc.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("append event %d to %q: %v", i, sid, err)
		}
	}
}

func (h *turnHarness) autoResumerWith(pred effects.Predicate, subAgents map[string]bool, window time.Duration) *autoResumer {
	return &autoResumer{
		runner:       h.runner,
		logger:       discardLogger(),
		store:        h.store,
		meters:       h.meters,
		wds:          h.wds,
		obs:          h.obs,
		tracker:      h.tracker,
		turnLocks:    h.locks,
		workloadName: "(test)",
		dispatchMode: "coordinator",
		pred:         pred,
		subAgents:    subAgents,
		window:       window,
	}
}

func candidateFor(t *testing.T, store *transcript.Store, sid string) transcript.InterruptedCandidate {
	t.Helper()
	cs, err := store.ScanInterrupted(context.Background())
	if err != nil {
		t.Fatalf("ScanInterrupted: %v", err)
	}
	for _, c := range cs {
		if c.SessionID == sid {
			return c
		}
	}
	t.Fatalf("no interrupted candidate for %q (found %d)", sid, len(cs))
	return transcript.InterruptedCandidate{}
}

// defaultPred classifies read_tool read-only and everything else (write_tool,
// MCP tools) mutating — the default-deny-unknown shape.
func defaultPred() effects.Predicate {
	return effects.NewPredicate(map[string]bool{"read_tool": false, "write_tool": true})
}

// --- tests -------------------------------------------------------------

// TestAutoResumeRepairsDanglingReadOnly: a read-only tool cut off
// mid-flight is answered with a synthetic error response and the session
// resumes; both the interruption marker and the attempt counter clear.
func TestAutoResumeRepairsDanglingReadOnly(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-repair", userText("hi"), modelCall("call-1", "read_tool"))
	if err := h.store.MarkInterrupted(ctx, "", "s-repair", "shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}

	ar := h.autoResumerWith(defaultPred(), nil, time.Hour)
	n := 0
	got := ar.resumeOne(ctx, candidateFor(t, h.store, "s-repair"), &n)
	if got != observability.AutoResumeResumed {
		t.Fatalf("outcome = %q, want %q", got, observability.AutoResumeResumed)
	}
	if n != 1 {
		t.Errorf("turnsThisBoot = %d, want 1", n)
	}
	if st := stateOf(t, h.store, "s-repair"); st != transcript.StateIdle {
		t.Errorf("state after resume = %q, want idle (marker cleared)", st)
	}
	if attempts, _ := h.store.AutoResumeAttempts(ctx, defaultUserID, "s-repair"); attempts != 0 {
		t.Errorf("attempt counter after resume = %d, want 0 (cleared)", attempts)
	}
}

// TestAutoResumeSkipsDanglingMutating (H1): a dangling mutating intent is
// ambiguous — the session is left for an operator ack, not resumed, and
// no attempt is counted.
func TestAutoResumeSkipsDanglingMutating(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-mut", userText("hi"), modelCall("call-w", "write_tool"))
	if err := h.store.MarkInterrupted(ctx, "", "s-mut", "shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}

	ar := h.autoResumerWith(defaultPred(), nil, time.Hour)
	before := h.eventCount(t, "s-mut")
	n := 0
	got := ar.resumeOne(ctx, candidateFor(t, h.store, "s-mut"), &n)
	if got != observability.AutoResumeSkippedAmbiguous {
		t.Fatalf("outcome = %q, want %q", got, observability.AutoResumeSkippedAmbiguous)
	}
	if st := stateOf(t, h.store, "s-mut"); st != transcript.StateInterrupted {
		t.Errorf("state = %q, want interrupted (marker left)", st)
	}
	if got := h.eventCount(t, "s-mut"); got != before {
		t.Errorf("event count %d -> %d: a turn ran on an ambiguous session", before, got)
	}
	if attempts, _ := h.store.AutoResumeAttempts(ctx, defaultUserID, "s-mut"); attempts != 0 {
		t.Errorf("attempt counter = %d, want 0 (no turn ran)", attempts)
	}
}

// TestAutoResumeCaseBReInvokes: a turn interrupted before the model ran
// (trailing user turn) is re-invoked over history with a nil message.
func TestAutoResumeCaseBReInvokes(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-caseb", userText("do the thing"))
	if err := h.store.MarkInterrupted(ctx, "", "s-caseb", "shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}

	ar := h.autoResumerWith(defaultPred(), nil, time.Hour)
	before := h.eventCount(t, "s-caseb")
	n := 0
	got := ar.resumeOne(ctx, candidateFor(t, h.store, "s-caseb"), &n)
	if got != observability.AutoResumeResumed {
		t.Fatalf("outcome = %q, want %q", got, observability.AutoResumeResumed)
	}
	if got := h.eventCount(t, "s-caseb"); got <= before {
		t.Errorf("event count %d -> %d: the continuation turn did not append", before, got)
	}
	if st := stateOf(t, h.store, "s-caseb"); st != transcript.StateIdle {
		t.Errorf("state after resume = %q, want idle", st)
	}
}

// TestAutoResumeClearsCompletedTurn (H2): a transcript that already ends
// on a model turn had actually completed (stale marker / clear race). The
// marker is cleared with NO extra model turn — re-running would inject
// ADK's synthetic "Continue processing…" and fabricate new work.
func TestAutoResumeClearsCompletedTurn(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-done", userText("hi"), modelText("all done"))
	if err := h.store.MarkInterrupted(ctx, "", "s-done", "shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}

	ar := h.autoResumerWith(defaultPred(), nil, time.Hour)
	before := h.eventCount(t, "s-done")
	n := 0
	got := ar.resumeOne(ctx, candidateFor(t, h.store, "s-done"), &n)
	if got != observability.AutoResumeCleared {
		t.Fatalf("outcome = %q, want %q", got, observability.AutoResumeCleared)
	}
	if got := h.eventCount(t, "s-done"); got != before {
		t.Errorf("event count %d -> %d: a spurious continuation turn ran", before, got)
	}
	if n != 0 {
		t.Errorf("turnsThisBoot = %d, want 0 (no turn driven)", n)
	}
	if st := stateOf(t, h.store, "s-done"); st != transcript.StateIdle {
		t.Errorf("state = %q, want idle (marker cleared)", st)
	}
}

// TestAutoResumeSkipsStale: an interruption older than the freshness
// window is left for an operator.
func TestAutoResumeSkipsStale(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-stale", userText("hi"))
	if err := h.store.MarkInterrupted(ctx, "", "s-stale", "shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}

	ar := h.autoResumerWith(defaultPred(), nil, time.Millisecond)
	time.Sleep(15 * time.Millisecond) // outlive the 1ms window
	n := 0
	got := ar.resumeOne(ctx, candidateFor(t, h.store, "s-stale"), &n)
	if got != observability.AutoResumeSkippedStale {
		t.Fatalf("outcome = %q, want %q", got, observability.AutoResumeSkippedStale)
	}
	if st := stateOf(t, h.store, "s-stale"); st != transcript.StateInterrupted {
		t.Errorf("state = %q, want interrupted (marker left)", st)
	}
}

// TestAutoResumeLoopBreaker (M2): a session already attempted the maximum
// number of times within the window is skipped, no turn.
func TestAutoResumeLoopBreaker(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-loop", userText("hi"))
	if err := h.store.MarkInterrupted(ctx, "", "s-loop", "shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}
	for i := 0; i < autoResumeMaxAttempts; i++ {
		if _, err := h.store.RecordAutoResumeAttempt(ctx, defaultUserID, "s-loop"); err != nil {
			t.Fatalf("RecordAutoResumeAttempt: %v", err)
		}
	}

	ar := h.autoResumerWith(defaultPred(), nil, time.Hour)
	before := h.eventCount(t, "s-loop")
	n := 0
	got := ar.resumeOne(ctx, candidateFor(t, h.store, "s-loop"), &n)
	if got != observability.AutoResumeSkippedLoopbreak {
		t.Fatalf("outcome = %q, want %q", got, observability.AutoResumeSkippedLoopbreak)
	}
	if got := h.eventCount(t, "s-loop"); got != before {
		t.Errorf("event count %d -> %d: a turn ran past the loop breaker", before, got)
	}
	if st := stateOf(t, h.store, "s-loop"); st != transcript.StateInterrupted {
		t.Errorf("state = %q, want interrupted (marker left)", st)
	}
}

// TestAutoResumeSupersession (M1): a session advanced by a concurrent
// inject after the scan is aborted at the preTurn recheck.
func TestAutoResumeSupersession(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-super", userText("hi"))
	if err := h.store.MarkInterrupted(ctx, "", "s-super", "shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}
	c := candidateFor(t, h.store, "s-super")

	// A concurrent actor advances the session after the scan snapshot.
	h.seed2Append(t, "s-super", modelText("someone else continued"))

	ar := h.autoResumerWith(defaultPred(), nil, time.Hour)
	n := 0
	got := ar.resumeOne(ctx, c, &n)
	if got != observability.AutoResumeSkippedSuperseded {
		t.Fatalf("outcome = %q, want %q", got, observability.AutoResumeSkippedSuperseded)
	}
	if st := stateOf(t, h.store, "s-super"); st != transcript.StateInterrupted {
		t.Errorf("state = %q, want interrupted (marker left)", st)
	}
}

// TestAutoResumeDeferredDelegation: a dangling sub-agent delegation is
// engine/operator territory in slice-1 — skipped as unsupported.
func TestAutoResumeDeferredDelegation(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-deleg", userText("hi"), modelCall("call-d", "sub_worker"))
	if err := h.store.MarkInterrupted(ctx, "", "s-deleg", "shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}

	ar := h.autoResumerWith(defaultPred(), map[string]bool{"sub_worker": true}, time.Hour)
	n := 0
	got := ar.resumeOne(ctx, candidateFor(t, h.store, "s-deleg"), &n)
	if got != observability.AutoResumeSkippedUnsupported {
		t.Fatalf("outcome = %q, want %q", got, observability.AutoResumeSkippedUnsupported)
	}
	if st := stateOf(t, h.store, "s-deleg"); st != transcript.StateInterrupted {
		t.Errorf("state = %q, want interrupted (marker left)", st)
	}
}

// TestAutoResumeMultiEventRepairableSkipped (H3): repairable calls that
// span more than one function-call event cannot be repaired with a single
// latest-function-response message, so the session is skipped.
func TestAutoResumeMultiEventRepairableSkipped(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-multi", userText("hi"), modelCall("call-a", "read_tool"), modelCall("call-b", "read_tool"))
	if err := h.store.MarkInterrupted(ctx, "", "s-multi", "shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}

	ar := h.autoResumerWith(defaultPred(), nil, time.Hour)
	n := 0
	got := ar.resumeOne(ctx, candidateFor(t, h.store, "s-multi"), &n)
	if got != observability.AutoResumeSkippedUnsupported {
		t.Fatalf("outcome = %q, want %q", got, observability.AutoResumeSkippedUnsupported)
	}
	if st := stateOf(t, h.store, "s-multi"); st != transcript.StateInterrupted {
		t.Errorf("state = %q, want interrupted (marker left)", st)
	}
}

// TestAutoResumeNonCoordinatorSkipped: slice-1 drives coordinator
// dispatch only; a graph-dispatch daemon leaves interrupted sessions.
func TestAutoResumeNonCoordinatorSkipped(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-graph", userText("hi"))
	if err := h.store.MarkInterrupted(ctx, "", "s-graph", "shutdown"); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}

	ar := h.autoResumerWith(defaultPred(), nil, time.Hour)
	ar.dispatchMode = "graph"
	n := 0
	got := ar.resumeOne(ctx, candidateFor(t, h.store, "s-graph"), &n)
	if got != observability.AutoResumeSkippedUnsupported {
		t.Fatalf("outcome = %q, want %q", got, observability.AutoResumeSkippedUnsupported)
	}
	if st := stateOf(t, h.store, "s-graph"); st != transcript.StateInterrupted {
		t.Errorf("state = %q, want interrupted (marker left)", st)
	}
}

// TestAutoResumeRunClearsAll: the boot pass over a mix drives each
// candidate and settles their markers.
func TestAutoResumeRunClearsAll(t *testing.T) {
	h := newTurnHarness(t, &blockableModel{})
	ctx := context.Background()
	h.seed(t, "s-run-b", userText("hi"))
	h.seed(t, "s-run-done", userText("hi"), modelText("finished"))
	for _, sid := range []string{"s-run-b", "s-run-done"} {
		if err := h.store.MarkInterrupted(ctx, "", sid, "shutdown"); err != nil {
			t.Fatalf("MarkInterrupted %q: %v", sid, err)
		}
	}

	ar := h.autoResumerWith(defaultPred(), nil, time.Hour)
	ar.run(ctx)

	for _, sid := range []string{"s-run-b", "s-run-done"} {
		if st := stateOf(t, h.store, sid); st != transcript.StateIdle {
			t.Errorf("state of %q after run = %q, want idle", sid, st)
		}
	}
}

// seed2Append appends one more event to an existing seeded session,
// simulating a concurrent writer advancing it after a scan snapshot.
func (h *turnHarness) seed2Append(t *testing.T, sid string, ev *adksession.Event) {
	t.Helper()
	ctx := context.Background()
	resp, err := h.svc.Get(ctx, &adksession.GetRequest{
		AppName: appName, UserID: defaultUserID, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("get %q for concurrent append: %v", sid, err)
	}
	if err := h.svc.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("concurrent append to %q: %v", sid, err)
	}
}
