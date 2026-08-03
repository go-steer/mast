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

package planner_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/transcript"
)

// TestPauseSessionParksAndRecordsToken pins the plane-A contract
// end-to-end (v0.2 pause/abort design): pause_session writes its
// token record — keyed by its own function-call ID — to the ops row,
// then parks the turn via LongRunningToolIDs, and the transcript
// projects the session paused with a resumable pending interrupt.
func TestPauseSessionParksAndRecordsToken(t *testing.T) {
	svc := session.InMemoryService()
	store := transcript.NewStore(svc, "planner-test")

	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolPauseSession, map[string]any{
			"reason":  "ambiguity",
			"message": "two conflicting rollout targets; need an operator decision",
		}),
	)
	root, err := planner.NewRoot(planner.Config{
		Name:          "w",
		Model:         plModel,
		PauseRecorder: store,
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, svc)
	events := runPlanner(t, r, "s-pause", genai.NewContentFromText("work", genai.RoleUser))

	var parkedID string
	for _, ev := range events {
		for _, id := range ev.LongRunningToolIDs {
			parkedID = id
		}
	}
	if parkedID == "" {
		t.Fatal("no LongRunningToolIDs event — pause_session did not park the run")
	}

	// The token record landed, keyed by the parked call's ID.
	records, err := store.ScanPauses(context.Background())
	if err != nil {
		t.Fatalf("ScanPauses: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("ScanPauses = %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.Plane != transcript.PlaneInterrupt || rec.InterruptID != parkedID || rec.SessionID != "s-pause" {
		t.Fatalf("record = %+v, want interrupt-plane record for parked call %s", rec, parkedID)
	}
	if rec.Reason != transcript.ReasonAmbiguity {
		t.Errorf("record reason = %q, want ambiguity", rec.Reason)
	}

	// The transcript projects paused (the H1 derivation extension) with
	// the park pending and resumable.
	d, err := store.Get(context.Background(), "op", "s-pause")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.State != transcript.StatePaused {
		t.Fatalf("state = %q, want paused", d.State)
	}
	if len(d.Pending) != 1 || d.Pending[0].InterruptID != parkedID || !d.Pending[0].LongRunning {
		t.Fatalf("pending = %+v, want the long-running park", d.Pending)
	}
}

// failingRecorder simulates the ops-row write failing.
type failingRecorder struct{}

func (failingRecorder) PauseInterrupt(context.Context, string, string, string, transcript.PauseSpec) (transcript.PauseHandle, error) {
	return transcript.PauseHandle{}, errors.New("ops row unavailable")
}

// TestPauseSessionRecordFailureDoesNotPark pins the M4 fail-safe: if
// the pause record cannot be written, the tool returns an error result
// instead of nil — no park without a token; the model sees the failure
// and the turn continues.
func TestPauseSessionRecordFailureDoesNotPark(t *testing.T) {
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolPauseSession, map[string]any{
			"reason": "ambiguity", "message": "x",
		}),
	)
	root, err := planner.NewRoot(planner.Config{
		Name:          "w",
		Model:         plModel,
		PauseRecorder: failingRecorder{},
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	svc := session.InMemoryService()
	r := newRunner(t, root, svc)
	runPlanner(t, r, "s-fail", genai.NewContentFromText("work", genai.RoleUser))

	// ADK stamps LongRunningToolIDs on the model-response event when
	// the call is EMITTED; what makes a park is the interrupt staying
	// unanswered. The error result answers it, so nothing may be
	// pending and the session must not project paused.
	store := transcript.NewStore(svc, "planner-test")
	d, err := store.Get(context.Background(), "op", "s-fail")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.State == transcript.StatePaused || len(d.Pending) > 0 {
		t.Fatalf("run parked despite the record-write failure — a tokenless park: state=%q pending=%+v", d.State, d.Pending)
	}
	// The model saw the structured error on the pause_session response.
	sawError := false
	for _, req := range plModel.requests() {
		for _, frs := range functionResponses(req)[planner.ToolPauseSession] {
			if frs["status"] == "error" {
				sawError = true
			}
		}
	}
	if !sawError {
		t.Fatal("model never saw the pause_session error result")
	}
}

// TestVocabularyIncludesPauseSessionWithRecorder: the tool joins the
// vocabulary only when a recorder exists (no recorder = no durable
// store = a pause that would die with the process).
func TestVocabularyIncludesPauseSessionWithRecorder(t *testing.T) {
	svc := session.InMemoryService()
	store := transcript.NewStore(svc, "planner-test")
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel) // finish immediately

	root, err := planner.NewRoot(planner.Config{Name: "w", Model: plModel, PauseRecorder: store})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, svc)
	runPlanner(t, r, "s-vocab", genai.NewContentFromText("work", genai.RoleUser))

	reqs := plModel.requests()
	if len(reqs) == 0 {
		t.Fatal("planner model never called")
	}
	if _, ok := reqs[0].Tools[planner.ToolPauseSession]; !ok {
		t.Fatalf("pause_session missing from the vocabulary with a recorder set; tools = %v", reqs[0].Tools)
	}
}
