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

package graph

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/providers/mock"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

const parkingToolName = "patch_k8s_resource"

// parkingSpecialist is a Task specialist holding one long-running tool
// whose nil return parks the turn — the shape the write gate produces
// when hitl.on_mutation asks about a mutating call
// (internal/llminternal writes an adk_request_confirmation call and
// stamps its ID into Event.LongRunningToolIDs; nothing here needs the
// gate itself, only the park it makes).
func parkingSpecialist(t *testing.T, name string, m model.LLM) adkagent.Agent {
	t.Helper()
	patch, err := functiontool.New(functiontool.Config{
		Name:          parkingToolName,
		Description:   "change a resource",
		IsLongRunning: true,
	}, func(adkagent.Context, struct{}) (map[string]any, error) {
		// nil result from a long-running tool IS the park.
		return nil, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	a, err := specialists.Build(specialists.Spec{
		Name: name, Description: name + " (test)", Mode: specialists.ModeTask,
		Instruction: "make the approved calls", OutputSchema: titleSchema,
	}, specialists.BuildOptions{Model: m, Tools: []tool.Tool{patch}})
	if err != nil {
		t.Fatalf("build specialist %q: %v", name, err)
	}
	return a
}

// parkTurn is one recorded model turn that calls the parking tool.
func parkTurn() mock.RecordedTurn {
	return recordedTurn(genai.NewPartFromFunctionCall(parkingToolName, map[string]any{}))
}

// parkingRoster: classifier, a diagnoser the echo model routes to, a
// change executor whose scripted model calls the parking tool, and the
// required fallback. require_approval is on, which is the condition
// #269 needs: the diagnoser's finding gate and the executor's write
// gate both apply to one incident.
func parkingRoster(t *testing.T, execModel, diagModel model.LLM) adkagent.Agent {
	t.Helper()
	diagnoser := buildSpec(t, "OOMKilled", specialists.ModeTask)
	if diagModel != nil {
		var err error
		diagnoser, err = specialists.Build(specialists.Spec{
			Name: "OOMKilled", Description: "OOMKilled (test)", Mode: specialists.ModeTask,
			Instruction: "test instruction",
		}, specialists.BuildOptions{Model: diagModel})
		if err != nil {
			t.Fatalf("build diagnoser: %v", err)
		}
	}
	root, err := Build(Config{
		Bundle: workload.Bundle{
			Name:        "w",
			Specialists: []string{"OOMKilled", "change-executor", FallbackName},
			HITL:        workload.HITL{RequireApproval: true},
		},
		Classifier: buildSpec(t, "clf", specialists.ModeSingleTurn),
		Specialists: map[string]Specialist{
			"OOMKilled": {Agent: diagnoser},
			"change-executor": {
				Agent:      parkingSpecialist(t, "change-executor", execModel),
				Capability: specialists.CapabilityChangeExecutor,
			},
			FallbackName: {Agent: buildSpec(t, FallbackName, specialists.ModeTask)},
		},
		ApprovedChangeSet: proposes(t, "OOMKilled"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return root
}

// turnParks is what one turn raised, split by who raised it: the
// RequestInput interrupt IDs the graph's own nodes asked for, and the
// long-running call IDs a specialist's tool parked on.
//
// Both arrive stamped into LongRunningToolIDs — NewRequestInputEvent
// parks the same way a long-running tool does — so the split is by
// whether the ID also names a RequestedInput. Conflating them is how
// this bug reads as one park when it is two.
type turnParks struct {
	requestInputs []string
	toolParks     []string
	// outputs is every terminal Output the turn produced, rendered. The
	// run's answer is in here when the graph reached `report`.
	outputs []string
}

func runTurn(t *testing.T, r *runner.Runner, msg *genai.Content) turnParks {
	t.Helper()
	var parks turnParks
	var longRunning []string
	asked := map[string]bool{}
	for ev, err := range r.Run(context.Background(), "op", "s1", msg, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev == nil {
			continue
		}
		if ev.RequestedInput != nil {
			parks.requestInputs = append(parks.requestInputs, ev.RequestedInput.InterruptID)
			asked[ev.RequestedInput.InterruptID] = true
		}
		longRunning = append(longRunning, ev.LongRunningToolIDs...)
		if ev.Output != nil {
			parks.outputs = append(parks.outputs, fmt.Sprint(ev.Output))
		}
	}
	for _, id := range longRunning {
		if !asked[id] {
			parks.toolParks = append(parks.toolParks, id)
		}
	}
	return parks
}

// inputAnswer is the resume message cmd/mast.resume builds for a
// RequestInput park: an adk_request_input FunctionResponse whose ID is
// the interrupt ID.
func inputAnswer(interruptID string, response map[string]any) *genai.Content {
	msg := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromFunctionResponse("adk_request_input", map[string]any{
			"response": response,
		})},
	}
	msg.Parts[0].FunctionResponse.ID = interruptID
	return msg
}

// TestAParkedSpecialistDoesNotAlsoRaiseItsNodesApprovalGate is #269.
//
// The executor's specialist parks at the write gate. workflow.RunNode
// reports a child that parked exactly as it reports a child that
// finished with no output — (nil, nil) — so the node read the park as a
// return and went on to raise its own result-approval interrupt on the
// same turn, over a result that does not exist yet ("Result: <nil>").
// Two parks, one turn, and answering either one strands the other: the
// approved write is never made.
func TestAParkedSpecialistDoesNotAlsoRaiseItsNodesApprovalGate(t *testing.T) {
	llm, err := mock.NewScripted(writeRecording(t, parkTurn()), false)
	if err != nil {
		t.Fatalf("NewScripted: %v", err)
	}
	root := parkingRoster(t, llm, nil)
	r, err := runner.New(runner.Config{
		AppName: "graph-writegate-park", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	// Turn 1: the incident. The diagnoser parks on its finding gate.
	first := runTurn(t, r, genai.NewContentFromText(
		`INJECT {"reason":"OOMKilled","namespace":"prod","name":"api"}`, genai.RoleUser))
	if len(first.requestInputs) != 1 || first.requestInputs[0] != "approve-OOMKilled" {
		t.Fatalf("turn 1 raised %v, want exactly [approve-OOMKilled]", first.requestInputs)
	}

	// Turn 2: the operator approves the finding, the executor runs, and
	// its write parks. That park is the only thing this turn may ask.
	second := runTurn(t, r, inputAnswer("approve-OOMKilled", map[string]any{"approved": true}))
	if len(second.toolParks) == 0 {
		t.Fatalf("the executor's write never parked; turn 2 raised %v", second.requestInputs)
	}
	for _, id := range second.requestInputs {
		if id == "approve-change-executor" {
			t.Fatalf("the executor's node raised %q on the same turn as its specialist's write park %v: "+
				"a specialist that parked was read as a specialist that returned, and answering either "+
				"interrupt strands the other (#269)", id, second.toolParks)
		}
	}
}

// confirmSeen wraps a model and reports whether it was ever sent a
// FunctionResponse for a named call — i.e. whether the operator's
// answer at the write gate actually reached the specialist that parked
// on it, rather than the specialist being re-asked from a history with
// no memory of its own call.
type confirmSeen struct {
	model.LLM
	mu   sync.Mutex
	name string
	seen bool
}

func (m *confirmSeen) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == m.name {
				m.seen = true
			}
		}
	}
	m.mu.Unlock()
	return m.LLM.GenerateContent(ctx, req, stream)
}

func (m *confirmSeen) confirmed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen
}

// TestTheApprovedWriteIsMadeOnceTheGateIsAnswered is the other half of
// #269 and the one the workload exists for: not just that the two parks
// stopped colliding, but that answering them in the order they are now
// raised carries the run to the end with the write made.
//
// Three turns, which is what the shape costs: the finding gate, the
// write gate, and the report. Turn 3 is a confirmation resume, and it
// re-enters the graph at START — so this also covers the upstream nodes
// surviving a turn whose content is a FunctionResponse rather than an
// incident.
func TestTheApprovedWriteIsMadeOnceTheGateIsAnswered(t *testing.T) {
	// Two recorded turns for the executor: the write, then the report it
	// makes once the write's result comes back.
	llm, err := mock.NewScripted(writeRecording(t, parkTurn(), finishTurn("applied")), false)
	if err != nil {
		t.Fatalf("NewScripted: %v", err)
	}
	exec := &confirmSeen{LLM: llm, name: parkingToolName}
	diag := &recordingModel{LLM: mastagent.NewEchoModel("echo-OOMKilled")}
	root := parkingRoster(t, exec, diag)
	r, err := runner.New(runner.Config{
		AppName: "graph-writegate-loop", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	runTurn(t, r, genai.NewContentFromText(
		`INJECT {"reason":"OOMKilled","namespace":"prod","name":"api"}`, genai.RoleUser))
	second := runTurn(t, r, inputAnswer("approve-OOMKilled", map[string]any{"approved": true}))
	if len(second.toolParks) != 1 {
		t.Fatalf("turn 2 parked on %v, want exactly one write gate", second.toolParks)
	}
	diagnosed := len(diag.prompts)

	// Turn 3: the operator answers the write gate. The response is the
	// tool's, not the node's — this is the shape a confirmed mutating
	// call comes back in.
	applied := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromFunctionResponse(parkingToolName, map[string]any{
			"status": "applied",
		})},
	}
	applied.Parts[0].FunctionResponse.ID = second.toolParks[0]

	third := runTurn(t, r, applied)
	if len(third.toolParks) != 0 {
		t.Fatalf("turn 3 parked again at %v; the confirmed write was not carried through", third.toolParks)
	}
	if len(third.requestInputs) != 0 {
		t.Fatalf("turn 3 asked %v; there is nothing left to approve once the write is confirmed", third.requestInputs)
	}
	// The executor reported on the write it made, and that report is the
	// run's answer. Without this the test would pass on a turn that did
	// nothing at all, which is precisely the failure #269 describes:
	// "the session goes idle having changed nothing".
	if !strings.Contains(strings.Join(third.outputs, "\n"), "applied") {
		t.Fatalf("turn 3 produced %v, want the executor's report on the write it made", third.outputs)
	}
	// And the report is about the write, not about a fresh start: the
	// specialist was handed the response to the very call it parked on.
	// Without this, a run that quietly re-asked the model to make the
	// change all over again would look identical from the outside.
	if !exec.confirmed() {
		t.Fatal("the executor never saw a response to its parked call; the operator's answer at the write gate did not reach the specialist that asked for it")
	}
	// The sticky verdict, stated as behaviour: turn 3 re-enters the
	// graph at START and passes back through the diagnoser's node, and
	// that node must recognize the answer it already has rather than
	// re-running the specialist to re-derive a finding and ask about it
	// again. Re-running it is not merely wasteful — a Task specialist
	// re-invoked on a turn whose content is a bare FunctionResponse is
	// the "at least one contents field is required" 400 in #269.
	if got := len(diag.prompts); got != diagnosed {
		t.Fatalf("the diagnoser ran %d more times on the confirmation resume (%d → %d); an answered finding gate must not re-derive its finding",
			got-diagnosed, diagnosed, got)
	}
}

// The write gate's park is a long-running tool call, and a diagnoser
// can meet it too — any specialist holding a tool this workload
// classifies as mutating does. So the guard is not executor-specific.
func TestParkedDiagnoserDoesNotAlsoRaiseItsFindingGate(t *testing.T) {
	llm, err := mock.NewScripted(writeRecording(t, parkTurn()), false)
	if err != nil {
		t.Fatalf("NewScripted: %v", err)
	}
	root, err := Build(Config{
		Bundle: workload.Bundle{
			Name:        "w",
			Specialists: []string{"OOMKilled", FallbackName},
			HITL:        workload.HITL{RequireApproval: true},
		},
		Classifier: buildSpec(t, "clf", specialists.ModeSingleTurn),
		Specialists: map[string]Specialist{
			"OOMKilled":  {Agent: parkingSpecialist(t, "OOMKilled", llm)},
			FallbackName: {Agent: buildSpec(t, FallbackName, specialists.ModeTask)},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "graph-writegate-park-diagnoser", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	parks := runTurn(t, r, genai.NewContentFromText(
		`INJECT {"reason":"OOMKilled","namespace":"prod","name":"api"}`, genai.RoleUser))
	if len(parks.toolParks) == 0 {
		t.Fatalf("the diagnoser's tool never parked; the turn raised %v", parks.requestInputs)
	}
	if len(parks.requestInputs) != 0 {
		t.Fatalf("the node raised %v while its specialist was parked at %v; the finding it would be "+
			"asking about has not been produced yet (#269)",
			parks.requestInputs, parks.toolParks)
	}
}
