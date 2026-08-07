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
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/a2a"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// newA2ABackend builds an a2aBackend over an in-memory transcript store
// with the given sessions pre-created (the runner creates sessions on
// first turn; tests seed them up front, mirroring trackerFixture).
func newA2ABackend(t *testing.T, sessionIDs ...string) (*a2aBackend, *transcript.Store) {
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
	obs := observability.New()
	return &a2aBackend{
		store:        store,
		obs:          obs,
		tracker:      newTurnTracker(store, discardLogger(), obs, "(test)"),
		logger:       discardLogger(),
		workloadName: "triage",
	}, store
}

// newA2ABackendRunner builds an a2aBackend backed by a real turn stack
// (runner + transcript store + tracker + locks + meters over an in-memory
// session service), so SubmitMessage can drive turns end-to-end through
// runTurnPre. workloadName is "(test)" to match the harness's primed obs.
func newA2ABackendRunner(t *testing.T, m model.LLM) *a2aBackend {
	t.Helper()
	h := newTurnHarness(t, m)
	return &a2aBackend{
		store:        h.store,
		obs:          h.obs,
		tracker:      h.tracker,
		logger:       discardLogger(),
		workloadName: "(test)",
		r:            h.runner,
		meters:       h.meters,
		wds:          h.wds,
		turnLocks:    h.locks,
		reg:          newTaskRegistry(),
	}
}

// TestA2ABackendSubmitMessageCompleted drives a message/send turn end to
// end: the model's answer is captured as the task Output, and the registry
// makes GetTask report "completed" — which the transcript projection alone
// (idle → working) never could. Neutralize check: drop the onEvent capture
// in runTurnPre and Output goes empty; drop the registry-first read in
// GetTask and the follow-up read reports working.
func TestA2ABackendSubmitMessageCompleted(t *testing.T) {
	b := newA2ABackendRunner(t, &blockableModel{})
	ctx := context.Background()

	taskID, info, err := b.SubmitMessage(ctx, a2a.SubmitParams{Text: "investigate", ContextID: "c1"})
	if err != nil {
		t.Fatalf("SubmitMessage: %v", err)
	}
	if !strings.HasPrefix(taskID, "a2a-") {
		t.Fatalf("task id = %q, want an a2a- prefix", taskID)
	}
	if info.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %q, want completed", info.State)
	}
	if info.Output != "done" {
		t.Fatalf("output = %q, want %q (the model's answer)", info.Output, "done")
	}
	if info.ContextID != "c1" {
		t.Fatalf("contextID = %q, want c1", info.ContextID)
	}
	got, err := b.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != a2a.TaskStateCompleted || got.Output != "done" {
		t.Fatalf("GetTask after submit = %+v, want completed/done from the registry", got)
	}
}

// TestA2ABackendSubmitMessageRefusesAborted: continuing an aborted task
// hits the runTurnPre chokepoint (ErrConflict), and classifyTurn projects
// the session's durable state (canceled) rather than "failed" — carrying
// no fabricated output.
func TestA2ABackendSubmitMessageRefusesAborted(t *testing.T) {
	b := newA2ABackendRunner(t, &blockableModel{})
	ctx := context.Background()

	taskID, _, err := b.SubmitMessage(ctx, a2a.SubmitParams{Text: "hi"})
	if err != nil {
		t.Fatalf("first SubmitMessage: %v", err)
	}
	if err := b.store.Abort(ctx, "", taskID, "operator abort"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	_, info, err := b.SubmitMessage(ctx, a2a.SubmitParams{TaskID: taskID, Text: "again"})
	if err != nil {
		t.Fatalf("SubmitMessage(aborted): %v", err)
	}
	if info.State != a2a.TaskStateCanceled {
		t.Fatalf("state = %q, want canceled (chokepoint refusal projected from the store)", info.State)
	}
	if info.Output != "" {
		t.Fatalf("refused task carried output %q, want empty", info.Output)
	}
}

// TestA2ABackendSubmitMessageDraining: once shutdown drain begins, new
// tasks are refused with ErrUnavailable (mirrors the inject door).
func TestA2ABackendSubmitMessageDraining(t *testing.T) {
	b := newA2ABackendRunner(t, &blockableModel{})
	b.tracker.beginDrain(context.Background())
	if _, _, err := b.SubmitMessage(context.Background(), a2a.SubmitParams{Text: "hi"}); !errors.Is(err, a2a.ErrUnavailable) {
		t.Fatalf("draining submit err = %v, want a2a.ErrUnavailable", err)
	}
}

// TestA2ABackendGetTaskRegistryWins: the in-process registry is
// authoritative over the transcript projection. A recorded "completed"
// record must win, though the idle session it names would otherwise
// project to "working". Neutralize check: remove the registry-first branch
// in GetTask and this reports working.
func TestA2ABackendGetTaskRegistryWins(t *testing.T) {
	b, _ := newA2ABackend(t, "a2a-live")
	b.reg = newTaskRegistry()
	b.reg.set("a2a-live", taskRecord{workload: "triage", state: a2a.TaskStateCompleted, output: "answer"})

	info, err := b.GetTask(context.Background(), "a2a-live")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if info.State != a2a.TaskStateCompleted || info.Output != "answer" {
		t.Fatalf("GetTask = %+v, want completed/answer from the registry", info)
	}
}

// TestA2ABackendCancelUpdatesRegistry: cancelling a task this process ran
// must overwrite its registry record, otherwise a stale "completed" would
// shadow the cancel on the next GetTask. Neutralize check: drop the
// reg.set in CancelTask and the post-cancel read still reports completed.
func TestA2ABackendCancelUpdatesRegistry(t *testing.T) {
	b := newA2ABackendRunner(t, &blockableModel{})
	ctx := context.Background()

	taskID, _, err := b.SubmitMessage(ctx, a2a.SubmitParams{Text: "hi"})
	if err != nil {
		t.Fatalf("SubmitMessage: %v", err)
	}
	if got, _ := b.GetTask(ctx, taskID); got.State != a2a.TaskStateCompleted {
		t.Fatalf("pre-cancel state = %q, want completed", got.State)
	}
	if _, err := b.CancelTask(ctx, taskID, "a2a tasks/cancel"); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	got, err := b.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask after cancel: %v", err)
	}
	if got.State != a2a.TaskStateCanceled {
		t.Fatalf("post-cancel GetTask = %q, want canceled (stale completed record shadowed the cancel)", got.State)
	}
}

// TestTaskRegistryRecordCancelWins pins the cancel-wins invariant: once a
// record is Canceled, a racing terminal write (a message/send turn that
// completed exactly as tasks/cancel landed) must not resurrect it —
// otherwise the model's answer leaks as a result artifact on a canceled
// task. record() must make the cancel authoritative regardless of write
// order. Neutralize check: make record an unconditional set and the
// cancel-then-complete case reports completed/answer.
func TestTaskRegistryRecordCancelWins(t *testing.T) {
	// complete, then cancel → cancel wins, no leaked output.
	r := newTaskRegistry()
	r.record("t", taskRecord{state: a2a.TaskStateCompleted, output: "answer"})
	r.record("t", taskRecord{state: a2a.TaskStateCanceled})
	if rec, _ := r.get("t"); rec.state != a2a.TaskStateCanceled || rec.output != "" {
		t.Fatalf("complete-then-cancel = %+v, want canceled/no-output", rec)
	}
	// cancel, then a racing completion → completion must NOT clobber it.
	r2 := newTaskRegistry()
	r2.record("t", taskRecord{state: a2a.TaskStateCanceled})
	r2.record("t", taskRecord{state: a2a.TaskStateCompleted, output: "answer"})
	if rec, _ := r2.get("t"); rec.state != a2a.TaskStateCanceled || rec.output != "" {
		t.Fatalf("cancel-then-complete = %+v, want canceled/no-output (cancel wins regardless of order)", rec)
	}
}

// TestTaskRegistryClearInFlight: a non-terminal in-flight record is dropped
// so GetTask falls back to the transcript (authoritative for
// working/input-required after a gate-paused refusal), but a terminal
// record (a racing cancel) survives. Neutralize check: make clearInFlight
// an unconditional delete and the terminal-survives case fails.
func TestTaskRegistryClearInFlight(t *testing.T) {
	r := newTaskRegistry()
	r.record("t", taskRecord{state: a2a.TaskStateWorking})
	r.clearInFlight("t")
	if _, ok := r.get("t"); ok {
		t.Fatal("clearInFlight left a working record; GetTask would shadow the transcript")
	}
	r.record("t", taskRecord{state: a2a.TaskStateCanceled})
	r.clearInFlight("t")
	if rec, ok := r.get("t"); !ok || rec.state != a2a.TaskStateCanceled {
		t.Fatalf("clearInFlight dropped a terminal record: %+v ok=%v", rec, ok)
	}
}

// TestA2ABackendRejectsForeignSession pins the ownership fence: the A2A
// surface addresses only tasks it minted (the "a2a-" prefix). A
// scope-holding caller must not read, cancel, or — the load-bearing case —
// drive a message/send turn into a session owned by another surface
// (inject "incident-*", attach, autoresume) by presenting its id as a task
// id. Neutralize check: relax isA2ATaskID to the old reserved-only check
// and SubmitMessage runs a turn into the foreign session (completed, nil
// err) instead of refusing.
func TestA2ABackendRejectsForeignSession(t *testing.T) {
	b := newA2ABackendRunner(t, &blockableModel{})
	ctx := context.Background()
	const foreign = "incident-op1" // an inject-owned session id

	if _, err := b.GetTask(ctx, foreign); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("GetTask(foreign) err = %v, want ErrTaskNotFound", err)
	}
	if _, err := b.CancelTask(ctx, foreign, "x"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("CancelTask(foreign) err = %v, want ErrTaskNotFound", err)
	}
	if _, _, err := b.SubmitMessage(ctx, a2a.SubmitParams{TaskID: foreign, Text: "hijack"}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("SubmitMessage(foreign) err = %v, want ErrTaskNotFound (turn injection into a foreign session)", err)
	}
}

// TestA2ABackendSubmitMintsContextID: when the caller omits contextId, the
// server assigns one (A2A v0.3) so follow-up messages can be grouped.
// Neutralize check: drop the mintContextID fallback and the returned
// contextId is empty.
func TestA2ABackendSubmitMintsContextID(t *testing.T) {
	b := newA2ABackendRunner(t, &blockableModel{})
	_, info, err := b.SubmitMessage(context.Background(), a2a.SubmitParams{Text: "hi"})
	if err != nil {
		t.Fatalf("SubmitMessage: %v", err)
	}
	if !strings.HasPrefix(info.ContextID, "ctx-") {
		t.Fatalf("contextID = %q, want a server-minted ctx- id", info.ContextID)
	}
}

// TestA2ABackendSubmitFailed: a genuine runner error (not a chokepoint
// ErrConflict) projects to "failed" with no fabricated output — the real
// path behind classifyTurn's default branch, which the fake-backend server
// test cannot reach. Neutralize check: change the default branch to
// completed and this reports completed.
func TestA2ABackendSubmitFailed(t *testing.T) {
	b := newA2ABackendRunner(t, errModel{})
	_, info, err := b.SubmitMessage(context.Background(), a2a.SubmitParams{Text: "hi"})
	if err != nil {
		t.Fatalf("SubmitMessage: %v", err)
	}
	if info.State != a2a.TaskStateFailed {
		t.Fatalf("state = %q, want failed", info.State)
	}
	if info.Output != "" {
		t.Fatalf("failed task carried output %q, want empty", info.Output)
	}
	// The internal runner error ("model exploded") must NOT reach the external
	// caller: the status is a generic string, the detail goes to the log.
	// Neutralize check: return err.Error() from classifyTurn's default branch
	// and this leaks the runner error.
	if info.StatusMessage != "task failed" {
		t.Fatalf("failed status = %q, want generic %q (internal error must not leak to the caller)", info.StatusMessage, "task failed")
	}
}

// TestA2ABackendSubmitCancelRaceNoLeak pins the cancel-wins invariant on the
// synchronous message/send *reply* (not just GetTask): a tasks/cancel that
// recorded canceled while the turn was in flight must win the reply too. The
// registry holds a canceled record but the store session is not aborted, so
// the turn completes with the model's answer — exactly the window where
// CancelTask's registry write landed between the turn passing the chokepoint
// and its terminal record. record() drops the completed write (cancel sticky),
// and SubmitMessage must return the registry's canceled view rather than leak
// the answer as a result artifact. Neutralize check: return the local snapshot
// instead of the registry view and this reports completed with output "done".
func TestA2ABackendSubmitCancelRaceNoLeak(t *testing.T) {
	b := newA2ABackendRunner(t, &blockableModel{})
	ctx := context.Background()
	b.reg.record("a2a-race", taskRecord{workload: b.workloadName, state: a2a.TaskStateCanceled, message: "operator cancel"})

	_, info, err := b.SubmitMessage(ctx, a2a.SubmitParams{TaskID: "a2a-race", Text: "hi"})
	if err != nil {
		t.Fatalf("SubmitMessage: %v", err)
	}
	if info.State != a2a.TaskStateCanceled {
		t.Fatalf("reply state = %q, want canceled (raced cancel wins the reply)", info.State)
	}
	if info.Output != "" {
		t.Fatalf("reply leaked output %q on a canceled task, want empty", info.Output)
	}
	got, err := b.GetTask(ctx, "a2a-race")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != a2a.TaskStateCanceled || got.Output != "" {
		t.Fatalf("GetTask = %+v, want canceled/no-output (reply must agree)", got)
	}
}

// TestDrainErrMapsUnavailable pins the drain-race mapping: if drain begins
// after SubmitMessage's pre-check while the turn is blocked on the per-session
// lock, runTurnPre returns inject.ErrUnavailable. SubmitMessage must surface
// that as the retryable a2a.ErrUnavailable (-32000), not fold it into a failed
// task; every other error falls through to classifyTurn. The branch itself is
// only reachable in the true production race (it shares the pre-check's
// isDraining predicate), so the mapping is factored into drainErr for a
// deterministic test. Neutralize check: return nil for ErrUnavailable and a
// drain-race task reports failed instead of retryable.
func TestDrainErrMapsUnavailable(t *testing.T) {
	if de := drainErr(fmt.Errorf("queued turn cancelled: %w", inject.ErrUnavailable)); !errors.Is(de, a2a.ErrUnavailable) {
		t.Fatalf("drainErr(ErrUnavailable) = %v, want a2a.ErrUnavailable", de)
	}
	if de := drainErr(errors.New("boom")); de != nil {
		t.Fatalf("drainErr(other) = %v, want nil (falls through to classifyTurn's failed)", de)
	}
	if de := drainErr(inject.ErrConflict); de != nil {
		t.Fatalf("drainErr(ErrConflict) = %v, want nil (aborted/paused is classifyTurn's job)", de)
	}
}

// TestTurnCaptureInputRequired: onEvent recognizes both HITL signals — a
// RequestedInput event (carrying its prompt) and an unanswered
// LongRunningToolIDs park. Neutralize check: drop either branch in onEvent
// and the corresponding case leaves inputRequired false.
func TestTurnCaptureInputRequired(t *testing.T) {
	var c turnCapture
	c.onEvent(&adksession.Event{RequestedInput: &adksession.RequestInput{Message: "approve?"}})
	if !c.inputRequired || c.interruptMsg != "approve?" {
		t.Fatalf("onEvent(RequestedInput): inputRequired=%v msg=%q", c.inputRequired, c.interruptMsg)
	}
	var c2 turnCapture
	c2.onEvent(&adksession.Event{LongRunningToolIDs: []string{"lr1"}})
	if !c2.inputRequired {
		t.Fatal("onEvent(LongRunningToolIDs) did not set inputRequired")
	}
}

// TestClassifyTurnInputRequired: a turn that completed with a HITL request
// pending projects to input-required with the prompt and no output — the
// classifyTurn branch the fake-backend server test cannot exercise.
// Neutralize check: drop the inputRequired branch and this reports
// completed with the (ignored) lastText leaking as output.
func TestClassifyTurnInputRequired(t *testing.T) {
	b, _ := newA2ABackend(t)
	cap := &turnCapture{inputRequired: true, interruptMsg: "approve?", lastText: "should-not-surface"}
	state, msg, out := b.classifyTurn(context.Background(), "a2a-x", cap, nil)
	if state != a2a.TaskStateInputRequired || msg != "approve?" || out != "" {
		t.Fatalf("classifyTurn(inputRequired) = %q/%q/%q, want input-required/approve?/empty", state, msg, out)
	}
}

func TestA2ABackendGetTask(t *testing.T) {
	b, store := newA2ABackend(t, "a2a-live", "a2a-gone")
	ctx := context.Background()

	// A reserved ops-row id on an owned task is never addressable.
	if _, err := b.GetTask(ctx, "a2a-live:mast-ops"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("GetTask(reserved) err = %v, want ErrTaskNotFound", err)
	}
	// A session owned by another surface (non-a2a- id) is not addressable.
	if _, err := b.GetTask(ctx, "incident-op1"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("GetTask(foreign) err = %v, want ErrTaskNotFound", err)
	}
	// An unknown but a2a-owned session maps ErrNotFound → ErrTaskNotFound.
	if _, err := b.GetTask(ctx, "a2a-nonexistent"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("GetTask(unknown) err = %v, want ErrTaskNotFound", err)
	}
	// An idle session is "working" (the log can't prove completion) and is
	// stamped with the backend's workload.
	info, err := b.GetTask(ctx, "a2a-live")
	if err != nil {
		t.Fatalf("GetTask(a2a-live): %v", err)
	}
	if info.State != a2a.TaskStateWorking || info.WorkloadName != "triage" {
		t.Fatalf("GetTask(a2a-live) = %+v, want working/triage", info)
	}
	// An aborted session projects to canceled with its reason.
	if err := store.Abort(ctx, "", "a2a-gone", "operator abort"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	info, err = b.GetTask(ctx, "a2a-gone")
	if err != nil {
		t.Fatalf("GetTask(aborted): %v", err)
	}
	if info.State != a2a.TaskStateCanceled || info.StatusMessage != "operator abort" {
		t.Fatalf("GetTask(aborted) = %+v, want canceled/operator abort", info)
	}
}

func TestA2ABackendCancelTask(t *testing.T) {
	b, _ := newA2ABackend(t, "a2a-s1")
	ctx := context.Background()

	// Reserved id, foreign-surface id, and unknown id all refuse as
	// not-found.
	if _, err := b.CancelTask(ctx, "a2a-s1:mast-ops", "x"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("CancelTask(reserved) err = %v, want ErrTaskNotFound", err)
	}
	if _, err := b.CancelTask(ctx, "incident-op1", "x"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("CancelTask(foreign) err = %v, want ErrTaskNotFound", err)
	}
	if _, err := b.CancelTask(ctx, "a2a-ghost", "x"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("CancelTask(unknown) err = %v, want ErrTaskNotFound", err)
	}

	// First cancel lands the terminal marker and reports canceled.
	info, err := b.CancelTask(ctx, "a2a-s1", "a2a tasks/cancel")
	if err != nil {
		t.Fatalf("CancelTask(a2a-s1): %v", err)
	}
	if info.State != a2a.TaskStateCanceled {
		t.Fatalf("first cancel state = %q, want canceled", info.State)
	}

	// Second cancel is idempotent: ErrAlreadyAborted maps to success and
	// still reports canceled (no spurious error surfaces to the caller).
	info, err = b.CancelTask(ctx, "a2a-s1", "a2a tasks/cancel")
	if err != nil {
		t.Fatalf("second CancelTask(a2a-s1): %v", err)
	}
	if info.State != a2a.TaskStateCanceled {
		t.Fatalf("idempotent cancel state = %q, want canceled", info.State)
	}
}

// TestA2AOutcomeVocabularyMatchesTaskStates pins the "must not drift"
// contract between the observability outcome constants (the primed label
// set for mast_a2a_server_tasks_total) and the pkg/a2a TaskState wire
// values the server records through them.
func TestA2AOutcomeVocabularyMatchesTaskStates(t *testing.T) {
	pairs := []struct {
		outcome string
		state   a2a.TaskState
	}{
		{observability.A2ATaskSubmitted, a2a.TaskStateSubmitted},
		{observability.A2ATaskWorking, a2a.TaskStateWorking},
		{observability.A2ATaskInputRequired, a2a.TaskStateInputRequired},
		{observability.A2ATaskCompleted, a2a.TaskStateCompleted},
		{observability.A2ATaskFailed, a2a.TaskStateFailed},
		{observability.A2ATaskCanceled, a2a.TaskStateCanceled},
		{observability.A2ATaskRejected, a2a.TaskStateRejected},
	}
	for _, p := range pairs {
		if p.outcome != string(p.state) {
			t.Fatalf("outcome %q != TaskState %q (the metric vocabulary drifted from the wire states)", p.outcome, p.state)
		}
	}
}

func TestMapTranscriptState(t *testing.T) {
	cases := []struct {
		name      string
		detail    *transcript.Detail
		wantState a2a.TaskState
		wantMsg   string
	}{
		{
			name:      "aborted maps to canceled",
			detail:    &transcript.Detail{Summary: transcript.Summary{State: transcript.StateAborted, AbortReason: "operator abort"}},
			wantState: a2a.TaskStateCanceled,
			wantMsg:   "operator abort",
		},
		{
			name: "paused with pending interrupt maps to input-required",
			detail: &transcript.Detail{Summary: transcript.Summary{
				State:               transcript.StatePaused,
				PendingInterruptIDs: []string{"i1"},
				PauseMessage:        "approve?",
			}},
			wantState: a2a.TaskStateInputRequired,
			wantMsg:   "approve?",
		},
		{
			name: "gate-only pause maps to working",
			detail: &transcript.Detail{Summary: transcript.Summary{
				State:        transcript.StatePaused,
				PauseMessage: "timed hold",
			}},
			wantState: a2a.TaskStateWorking,
			wantMsg:   "timed hold",
		},
		{
			name:      "interrupted maps to working",
			detail:    &transcript.Detail{Summary: transcript.Summary{State: transcript.StateInterrupted, InterruptReason: "shutdown"}},
			wantState: a2a.TaskStateWorking,
			wantMsg:   "shutdown",
		},
		{
			name:      "idle maps to working (never completed from the log)",
			detail:    &transcript.Detail{Summary: transcript.Summary{State: transcript.StateIdle}},
			wantState: a2a.TaskStateWorking,
			wantMsg:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, msg := mapTranscriptState(tc.detail)
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q", state, tc.wantState)
			}
			if msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", msg, tc.wantMsg)
			}
		})
	}
}

func TestMapTranscriptStateNeverCompleted(t *testing.T) {
	// The transcript cannot prove a turn finished; no projection may ever
	// return the "completed" A2A state (that comes from Stage B's registry).
	for _, s := range []string{transcript.StateIdle, transcript.StatePaused, transcript.StateInterrupted, transcript.StateAborted} {
		if got, _ := mapTranscriptState(&transcript.Detail{Summary: transcript.Summary{State: s}}); got == a2a.TaskStateCompleted {
			t.Fatalf("state %q mapped to completed", s)
		}
	}
}

func TestA2AExposedSkills(t *testing.T) {
	// Not opted in → no skills.
	if got := a2aExposedSkills(&workload.Bundle{Name: "w"}); got != nil {
		t.Fatalf("expose:false: got %v, want nil", got)
	}
	if got := a2aExposedSkills(nil); got != nil {
		t.Fatalf("nil bundle: got %v, want nil", got)
	}

	// Opted in with defaults: skill name and description fall back to the
	// workload name/description.
	b := &workload.Bundle{
		Name:        "triage",
		Description: "GKE triage",
		A2A:         workload.A2A{Expose: true, Auth: workload.A2AAuth{Scopes: []string{"triage:invoke"}}},
	}
	got := a2aExposedSkills(b)
	if len(got) != 1 {
		t.Fatalf("got %d skills, want 1", len(got))
	}
	sk := got[0]
	if sk.WorkloadName != "triage" || sk.SkillName != "triage" || sk.Description != "GKE triage" {
		t.Fatalf("skill = %+v", sk)
	}
	if len(sk.Scopes) != 1 || sk.Scopes[0] != "triage:invoke" {
		t.Fatalf("scopes = %v", sk.Scopes)
	}

	// Explicit skill name/description win.
	b.A2A.SkillName = "gke-triage"
	b.A2A.SkillDescription = "triage alerts"
	got = a2aExposedSkills(b)
	if got[0].SkillName != "gke-triage" || got[0].Description != "triage alerts" {
		t.Fatalf("explicit skill = %+v", got[0])
	}
}

func TestA2AValidator(t *testing.T) {
	skills := []a2a.ExposedSkill{
		{WorkloadName: "a", SkillName: "a", Scopes: []string{"a:invoke", "shared"}},
		{WorkloadName: "b", SkillName: "b", Scopes: []string{"b:invoke", "shared"}},
	}
	logger := newLogger("error")

	// Unset token → no validator (dev-only open access).
	t.Setenv("MAST_A2A_TOKEN", "")
	v, err := a2aValidator(logger, skills)
	if err != nil {
		t.Fatalf("a2aValidator(unset): %v", err)
	}
	if v != nil {
		t.Fatal("unset token: want nil validator")
	}

	// Set token → static validator whose principal carries the union of
	// every exposed skill's scopes.
	t.Setenv("MAST_A2A_TOKEN", "secret")
	v, err = a2aValidator(logger, skills)
	if err != nil {
		t.Fatalf("a2aValidator(set): %v", err)
	}
	p, err := v.Validate(t.Context(), "secret")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, want := range []string{"a:invoke", "b:invoke", "shared"} {
		if !hasScopeTest(p.Scopes, want) {
			t.Fatalf("principal missing scope %q (have %v)", want, p.Scopes)
		}
	}
	// "shared" appears in both skills but must be de-duplicated.
	if n := countTest(p.Scopes, "shared"); n != 1 {
		t.Fatalf("scope \"shared\" appears %d times, want 1", n)
	}
}

func TestA2ARateLimiter(t *testing.T) {
	logger := newLogger("error")

	// Unset rate → no limiter (limiting off, mirrors the auth seam).
	t.Setenv("MAST_A2A_RATE", "")
	t.Setenv("MAST_A2A_BURST", "")
	lim, err := a2aRateLimiter(logger)
	if err != nil {
		t.Fatalf("a2aRateLimiter(unset): %v", err)
	}
	if lim != nil {
		t.Fatal("unset rate: want nil limiter")
	}

	// Valid rate → limiter; burst defaults to ceil(rate).
	t.Setenv("MAST_A2A_RATE", "2.5")
	lim, err = a2aRateLimiter(logger)
	if err != nil {
		t.Fatalf("a2aRateLimiter(2.5): %v", err)
	}
	if lim == nil {
		t.Fatal("valid rate: want limiter, got nil")
	}

	// Explicit burst overrides the derived default.
	t.Setenv("MAST_A2A_BURST", "10")
	if _, err := a2aRateLimiter(logger); err != nil {
		t.Fatalf("a2aRateLimiter(burst=10): %v", err)
	}

	// Fail closed on every hostile input rather than silently disabling.
	t.Setenv("MAST_A2A_BURST", "")
	for _, bad := range []string{"abc", " 5 ", "Inf", "NaN", "0", "-1", "1e400"} {
		t.Setenv("MAST_A2A_RATE", bad)
		if _, err := a2aRateLimiter(logger); err == nil {
			t.Errorf("MAST_A2A_RATE=%q: want error, got nil", bad)
		}
	}

	// A malformed explicit burst also fails closed.
	t.Setenv("MAST_A2A_RATE", "5")
	for _, bad := range []string{"abc", "0", "-1"} {
		t.Setenv("MAST_A2A_BURST", bad)
		if _, err := a2aRateLimiter(logger); err == nil {
			t.Errorf("MAST_A2A_BURST=%q: want error, got nil", bad)
		}
	}
}

// TestA2ABackendStreamMessageEmitsProgress drives a message/stream turn
// end to end over the real turn stack: the backend emits the initial Task
// snapshot first, then a working status-update carrying the model's text as
// progress, and returns the terminal snapshot (completed, output "done")
// the server bookends the stream from. Neutralize checks: drop the initial
// Task emit and the first frame is no longer a *a2a.Task; drop
// emitStreamProgress and no status-update carries the model text.
func TestA2ABackendStreamMessageEmitsProgress(t *testing.T) {
	b := newA2ABackendRunner(t, &blockableModel{})
	ctx := context.Background()

	var emitted []any
	emit := func(ev any) { emitted = append(emitted, ev) }
	taskID, info, err := b.StreamMessage(ctx, a2a.SubmitParams{Text: "investigate", ContextID: "c1"}, emit)
	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}
	if !strings.HasPrefix(taskID, "a2a-") {
		t.Fatalf("task id = %q, want an a2a- prefix", taskID)
	}
	if info.State != a2a.TaskStateCompleted || info.Output != "done" {
		t.Fatalf("terminal snapshot = %+v, want completed/done", info)
	}
	if len(emitted) < 2 {
		t.Fatalf("emitted %d events, want >= 2 (initial task + progress)", len(emitted))
	}
	// The first emit is the initial Task snapshot (state working), carrying
	// the resolved task/context ids.
	task, ok := emitted[0].(*a2a.Task)
	if !ok {
		t.Fatalf("first emit = %T, want *a2a.Task", emitted[0])
	}
	if task.ID != taskID || task.ContextID != "c1" || task.Status.State != a2a.TaskStateWorking {
		t.Fatalf("initial task = %+v, want %s/c1/working", task, taskID)
	}
	// A working status-update surfaces the model's text as progress, keyed to
	// the same task/context, marked non-final.
	var gotProgress bool
	for _, ev := range emitted[1:] {
		su, ok := ev.(*a2a.TaskStatusUpdateEvent)
		if !ok {
			continue
		}
		if su.Final {
			t.Fatalf("backend emitted a final=true status-update; the terminal frame is the server's job: %+v", su)
		}
		if su.TaskID != taskID || su.ContextID != "c1" || su.Status.State != a2a.TaskStateWorking {
			t.Fatalf("progress update = %+v, want %s/c1/working", su, taskID)
		}
		if su.Status.Message != nil && len(su.Status.Message.Parts) > 0 && su.Status.Message.Parts[0].Text == "done" {
			gotProgress = true
		}
	}
	if !gotProgress {
		t.Fatal("no working status-update carried the model text; emitStreamProgress did not fire")
	}
}

// TestEmitStreamProgressSkipsNonModelAndEmpty pins emitStreamProgress's two
// skip branches and the per-frame messageId. A nil event, a non-model (user)
// event, and empty-text model events must all yield zero frames; only
// model-authored text ticks the stream, and each frame gets a distinct
// messageId (seq) so a deduping client cannot collapse a multi-round turn.
// Neutralize checks: drop the `Role != RoleModel` guard and the user event
// leaks a frame; drop the `sb.Len() == 0` guard and the empty events leak
// frames; make the messageId a fixed suffix and the distinct-id assertion
// fails.
func TestEmitStreamProgressSkipsNonModelAndEmpty(t *testing.T) {
	var got []*a2a.TaskStatusUpdateEvent
	emit := func(ev any) {
		su, ok := ev.(*a2a.TaskStatusUpdateEvent)
		if !ok {
			t.Fatalf("emit got %T, want *a2a.TaskStatusUpdateEvent", ev)
		}
		got = append(got, su)
	}
	mkEv := func(c *genai.Content) *adksession.Event {
		ev := adksession.NewEvent(context.Background(), "inv-test")
		ev.Content = c
		return ev
	}
	// nil, non-model, and empty-text model events must all be skipped.
	emitStreamProgress(emit, "a2a-x", "c1", 0, nil)
	emitStreamProgress(emit, "a2a-x", "c1", 1, mkEv(genai.NewContentFromText("user says hi", genai.RoleUser)))
	emitStreamProgress(emit, "a2a-x", "c1", 2, mkEv(&genai.Content{Role: genai.RoleModel}))
	emitStreamProgress(emit, "a2a-x", "c1", 3, mkEv(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: ""}}}))
	if len(got) != 0 {
		t.Fatalf("skipped events emitted %d frames, want 0: %+v", len(got), got)
	}
	// Two model-text events → two working, non-final frames with distinct ids.
	emitStreamProgress(emit, "a2a-x", "c1", 4, mkEv(genai.NewContentFromText("first", genai.RoleModel)))
	emitStreamProgress(emit, "a2a-x", "c1", 5, mkEv(genai.NewContentFromText("second", genai.RoleModel)))
	if len(got) != 2 {
		t.Fatalf("two model-text events emitted %d frames, want 2", len(got))
	}
	if got[0].TaskID != "a2a-x" || got[0].ContextID != "c1" || got[0].Status.State != a2a.TaskStateWorking || got[0].Final {
		t.Fatalf("progress frame = %+v, want a2a-x/c1/working/non-final", got[0])
	}
	if got[0].Status.Message.Parts[0].Text != "first" || got[1].Status.Message.Parts[0].Text != "second" {
		t.Fatalf("progress texts = %q,%q, want first,second", got[0].Status.Message.Parts[0].Text, got[1].Status.Message.Parts[0].Text)
	}
	if got[0].Status.Message.MessageID == got[1].Status.Message.MessageID {
		t.Fatalf("progress frames share messageId %q; a deduping client would collapse them", got[0].Status.Message.MessageID)
	}
}

// TestA2ABackendStreamForeignSessionNoEmit pins the ownership fence on the
// stream path: a continuation targeting a foreign-surface session id
// returns ErrTaskNotFound BEFORE any emit, so the server reports a clean
// JSON-RPC error rather than an orphaned SSE stream. Neutralize check: move
// the initial Task emit above the id validation and this leaks a frame.
func TestA2ABackendStreamForeignSessionNoEmit(t *testing.T) {
	b := newA2ABackendRunner(t, &blockableModel{})
	var emitted int
	emit := func(any) { emitted++ }
	_, _, err := b.StreamMessage(context.Background(), a2a.SubmitParams{TaskID: "incident-op1", Text: "hijack"}, emit)
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("StreamMessage(foreign) err = %v, want ErrTaskNotFound", err)
	}
	if emitted != 0 {
		t.Fatalf("foreign continuation emitted %d frames before the not-found refusal, want 0", emitted)
	}
}

// TestA2ABackendStreamDrainingNoEmit pins the same pre-emit posture for the
// drain gate: a draining server refuses a fresh stream with ErrUnavailable
// and emits nothing, so the refusal rides a retryable JSON-RPC error, not a
// half-open stream.
func TestA2ABackendStreamDrainingNoEmit(t *testing.T) {
	b := newA2ABackendRunner(t, &blockableModel{})
	b.tracker.beginDrain(context.Background())
	var emitted int
	emit := func(any) { emitted++ }
	_, _, err := b.StreamMessage(context.Background(), a2a.SubmitParams{Text: "hi"}, emit)
	if !errors.Is(err, a2a.ErrUnavailable) {
		t.Fatalf("StreamMessage(draining) err = %v, want ErrUnavailable", err)
	}
	if emitted != 0 {
		t.Fatalf("draining stream emitted %d frames, want 0 (refusal precedes any emit)", emitted)
	}
}

func hasScopeTest(scopes []string, want string) bool {
	return countTest(scopes, want) > 0
}

func countTest(scopes []string, want string) int {
	n := 0
	for _, s := range scopes {
		if s == want {
			n++
		}
	}
	return n
}
