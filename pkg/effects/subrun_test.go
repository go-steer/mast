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

package effects

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/genai"

	adksession "google.golang.org/adk/v2/session"
)

// fakeIntentStore records what the recorder asked it to persist.
type fakeIntentStore struct {
	recorded        []DanglingIntent
	specialistsSeen []string
	completed       []string
	recordErr       error
	doneErr         error
}

func (f *fakeIntentStore) RecordSubRunIntents(_ context.Context, _, specialist string, intents []DanglingIntent) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, intents...)
	f.specialistsSeen = append(f.specialistsSeen, specialist)
	return nil
}

func (f *fakeIntentStore) CompleteSubRunIntents(_ context.Context, _ string, callIDs []string) error {
	if f.doneErr != nil {
		return f.doneErr
	}
	f.completed = append(f.completed, callIDs...)
	return nil
}

func subRunEvent(inv string, parts ...*genai.Part) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), inv)
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
	ev.Timestamp = time.Now()
	return ev
}

func fcPart(name, id string) *genai.Part {
	p := genai.NewPartFromFunctionCall(name, map[string]any{})
	p.FunctionCall.ID = id
	return p
}

func frPart(name, id string) *genai.Part {
	p := genai.NewPartFromFunctionResponse(name, map[string]any{"ok": true})
	p.FunctionResponse.ID = id
	return p
}

func TestMutatingCallsExclusions(t *testing.T) {
	// list_pods read-only by override; scale_up mutating by default;
	// invoke_specialist spawning; triage_bot a composed sub-agent.
	pred := NewPredicate(map[string]bool{"list_pods": false})
	subAgents := map[string]bool{"triage_bot": true}

	ev := subRunEvent("inv1",
		fcPart("scale_up", "c1"),          // mutating: recorded
		fcPart("invoke_specialist", "c2"), // spawning: recorded
		fcPart("list_pods", "c3"),         // read-only: skipped
		fcPart("adk_request_input", "c4"), // engine control: skipped
		fcPart("triage_bot", "c5"),        // delegation: skipped
		fcPart("scale_up", ""),            // unkeyable: skipped
		fcPart("restart_pod", "lr1"),      // long-running park: skipped
	)
	ev.LongRunningToolIDs = []string{"lr1"}

	raised, completed := MutatingCalls(ev, pred, subAgents)
	if len(raised) != 2 || raised[0].CallID != "c1" || raised[1].CallID != "c2" {
		t.Fatalf("raised = %+v, want just c1 (mutating) and c2 (spawning)", raised)
	}
	if raised[0].ToolName != "scale_up" || raised[0].InvocationID != "inv1" {
		t.Errorf("raised[0] = %+v, want the scale_up call carrying its invocation", raised[0])
	}
	// Not from this session's log, so there is no index into it.
	if raised[0].EventIndex != -1 {
		t.Errorf("raised[0].EventIndex = %d, want -1 — an external intent indexes no event in this log", raised[0].EventIndex)
	}
	if len(completed) != 0 {
		t.Errorf("completed = %v, want none from a call-only event", completed)
	}

	// The response side pairs by ID regardless of class: the recorder
	// only ever holds records for calls it raised, so a completion for
	// an unrecorded ID is a harmless no-op downstream.
	_, completed = MutatingCalls(subRunEvent("inv1", frPart("scale_up", "c1")), pred, subAgents)
	if len(completed) != 1 || completed[0] != "c1" {
		t.Fatalf("completed = %v, want [c1]", completed)
	}
}

func TestSubRunRecorderRecordsThenPairs(t *testing.T) {
	store := &fakeIntentStore{}
	rec, err := NewSubRunRecorder(SubRunRecorderConfig{
		Store:     store,
		SessionID: "s1",
		// The specialist's name, because the outer log cannot carry it:
		// from there the whole dispatch is one invoke_specialist call.
		Specialist: "remediator",
		Predicate:  NewPredicate(nil),
	})
	if err != nil {
		t.Fatalf("NewSubRunRecorder: %v", err)
	}

	if err := rec.Observe(subRunEvent("inv1", fcPart("scale_up", "c1"))); err != nil {
		t.Fatalf("Observe call: %v", err)
	}
	if len(store.recorded) != 1 || store.recorded[0].CallID != "c1" {
		t.Fatalf("recorded = %+v, want the c1 intent", store.recorded)
	}
	if len(store.specialistsSeen) != 1 || store.specialistsSeen[0] != "remediator" {
		t.Errorf("specialist = %v, want [remediator]", store.specialistsSeen)
	}

	if err := rec.Observe(subRunEvent("inv1", frPart("scale_up", "c1"))); err != nil {
		t.Fatalf("Observe response: %v", err)
	}
	if len(store.completed) != 1 || store.completed[0] != "c1" {
		t.Fatalf("completed = %v, want [c1]", store.completed)
	}

	rec.Close() // nothing to flush; every record is written as it arrives
}

// TestSubRunRecorderStopsTheDispatchOnRecordFailure pins the divergence
// from the rest of the sink contract: a metering hiccup is swallowed,
// an unrecordable mutation is not. Under on_mutation: apply the record
// is the only control the call has, and a control that fails open is
// not one.
func TestSubRunRecorderStopsTheDispatchOnRecordFailure(t *testing.T) {
	boom := errors.New("ops row unwritable")
	store := &fakeIntentStore{recordErr: boom}
	rec, err := NewSubRunRecorder(SubRunRecorderConfig{
		Store: store, SessionID: "s1", Specialist: "remediator",
		Predicate: NewPredicate(map[string]bool{"list_pods": false}),
	})
	if err != nil {
		t.Fatalf("NewSubRunRecorder: %v", err)
	}

	err = rec.Observe(subRunEvent("inv1", fcPart("scale_up", "c1")))
	if err == nil {
		t.Fatal("Observe returned nil after a failed intent write; the dispatch must stop")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Observe error = %v, want it to wrap the store failure", err)
	}

	// A read-only call over the same broken store is unaffected: only
	// the calls this seam exists for are held to it.
	if err := rec.Observe(subRunEvent("inv1", fcPart("list_pods", "c2"))); err != nil {
		t.Errorf("a read-only call must not be stopped by an unwritable store: %v", err)
	}
}

// A failed COMPLETION is the opposite case: by then the call has
// happened and refusing cannot un-happen it, so the intent is left
// dangling and the dispatch continues.
func TestSubRunRecorderSwallowsCompletionFailure(t *testing.T) {
	store := &fakeIntentStore{doneErr: errors.New("ops row unwritable")}
	rec, err := NewSubRunRecorder(SubRunRecorderConfig{
		Store: store, SessionID: "s1", Specialist: "remediator", Predicate: NewPredicate(nil),
	})
	if err != nil {
		t.Fatalf("NewSubRunRecorder: %v", err)
	}
	if err := rec.Observe(subRunEvent("inv1", frPart("scale_up", "c1"))); err != nil {
		t.Fatalf("Observe response = %v; a failed completion must not stop the dispatch", err)
	}
}

// The recorder records against a session, so it refuses to be built
// without one rather than writing dispatched mutations into the void.
func TestNewSubRunRecorderRequiresASession(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  SubRunRecorderConfig
	}{
		{"no store", SubRunRecorderConfig{SessionID: "s1", Predicate: NewPredicate(nil)}},
		{"no session", SubRunRecorderConfig{Store: &fakeIntentStore{}, Predicate: NewPredicate(nil)}},
		{"no predicate", SubRunRecorderConfig{Store: &fakeIntentStore{}, SessionID: "s1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSubRunRecorder(tc.cfg); err == nil {
				t.Fatal("NewSubRunRecorder returned no error")
			}
		})
	}
}

// TestWithExternalJoinsMutatingOnly is what makes a killed dispatch
// visible to the workload's own boot pass: the intents are absent from
// the log ScanDangling read, and they must land in the bucket that
// blocks auto-resume rather than the one that gets a synthetic answer.
func TestWithExternalJoinsMutatingOnly(t *testing.T) {
	base := time.Now()
	inLog := DanglingIntent{ToolName: "scale_up", CallID: "c1", Timestamp: base.Add(2 * time.Second), EventIndex: 4}
	repairable := DanglingIntent{ToolName: "list_pods", CallID: "c0", Timestamp: base, EventIndex: 1}
	scan := DanglingScan{
		Mutating:           []DanglingIntent{inLog},
		Repairable:         []DanglingIntent{repairable},
		LastCallEventIndex: 4,
	}
	external := []DanglingIntent{
		{ToolName: "restart_pod", CallID: "x1", Timestamp: base.Add(time.Second), EventIndex: -1},
	}

	got := scan.WithExternal(external)
	if len(got.Mutating) != 2 {
		t.Fatalf("Mutating = %+v, want the logged and the external intent", got.Mutating)
	}
	// Merged in time order, not appended: every consumer reads these as
	// a sequence.
	if got.Mutating[0].CallID != "x1" || got.Mutating[1].CallID != "c1" {
		t.Errorf("Mutating order = %q,%q; want x1,c1 (oldest first)", got.Mutating[0].CallID, got.Mutating[1].CallID)
	}
	if len(got.Repairable) != 1 || got.Repairable[0].CallID != "c0" {
		t.Errorf("Repairable = %+v; an external intent must never become repairable — the sub-session that made the call is gone, so there is nothing to answer", got.Repairable)
	}
	if got.LastCallEventIndex != 4 {
		t.Errorf("LastCallEventIndex = %d, want 4 unchanged", got.LastCallEventIndex)
	}

	// The original scan is untouched — callers hold it across the fold.
	if len(scan.Mutating) != 1 {
		t.Errorf("WithExternal mutated its receiver: %+v", scan.Mutating)
	}
	if got := scan.WithExternal(nil); len(got.Mutating) != 1 {
		t.Errorf("WithExternal(nil) = %+v, want the scan unchanged", got.Mutating)
	}
}

// TestExternalDanglingRefusesMutatingCalls is the payoff of the whole
// seam: a dispatch killed mid-mutation leaves NOTHING in the outer
// session's log — that is the isolation the dispatch boundary is for —
// so without the external fold the next turn reads a clean session and
// happily mutates again. With it, the session is in ambiguous-effect
// mode exactly as if the dangling call had been its own.
func TestExternalDanglingRefusesMutatingCalls(t *testing.T) {
	external := []DanglingIntent{
		{ToolName: "scale_up", CallID: "dispatched-1", Timestamp: time.Now().Add(-time.Minute), EventIndex: -1},
	}
	h := newHarnessWith(t, roundScript(
		callResponse("scale_up", map[string]any{"delta": 3}),
		callResponse("list_pods", map[string]any{}),
	), func(context.Context, string) []DanglingIntent { return external })

	events := h.run(t, "dispatch-wounded", "continue the work")

	h.mu.Lock()
	scale, list := h.scale, h.list
	h.mu.Unlock()
	if scale != 0 {
		t.Fatalf("scale_up executed %d times after an interrupted dispatch, want 0 (fail-closed)", scale)
	}
	if list != 1 {
		t.Fatalf("list_pods executed %d times, want 1 (read-only tools proceed in ambiguous mode)", list)
	}
	resps := toolResponses(events)
	if got := resps["scale_up"]; len(got) != 1 || got[0]["error"] != "ambiguous_prior_effect" {
		t.Fatalf("scale_up response = %v, want one ambiguous_prior_effect refusal", got)
	}
}

// The fold happens before the ack filter, so `mast ack-effects` clears
// a dispatched dangling intent on the same terms as an in-band one.
// Without that ordering an interrupted dispatch would wedge a session
// with no operator escape at all.
func TestAckLiftsExternalDanglingMode(t *testing.T) {
	external := []DanglingIntent{
		{ToolName: "scale_up", CallID: "dispatched-2", Timestamp: time.Now().Add(-time.Minute), EventIndex: -1},
	}
	h := newHarnessWith(t, roundScript(callResponse("scale_up", map[string]any{"delta": 4})),
		func(context.Context, string) []DanglingIntent { return external })

	if err := h.store.AckEffects(context.Background(), testUser, "dispatch-acked", "operator checked the cluster"); err != nil {
		t.Fatalf("AckEffects: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	h.run(t, "dispatch-acked", "continue after ack")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale != 1 {
		t.Fatalf("scale_up executed %d times after operator ack, want 1 (an ack must reach a dispatched intent too)", h.scale)
	}
}
