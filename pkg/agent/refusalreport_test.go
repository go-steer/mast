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

// What a refused Task agent reports, and the loop it must not enter.
//
// W10.2 shipped the refusal as a finish_task call and W10.3 stopped the
// session being cancelled over one, which together exposed a case
// neither had measured: an agent that declares an OutputSchema has a
// finish_task whose parameters *are* that schema, and ADK answers an
// invalid finish_task with a retry instruction rather than an error. A
// refused agent that retries is refused again and submits the same
// invalid report. Nothing in that circuit costs a model call, so nothing
// in it ends — the v0.4 UAT's producer-contract leg spun to 3,292
// finish_task calls before its harness gave up.
package agent_test

import (
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

// findingSchema stands in for a roster's output_schema: an object with a
// required field that is not "result", which is all it takes to make
// DefaultRefusalPayload invalid against it.
func findingSchema() *genai.Schema {
	return &genai.Schema{
		Type:       genai.TypeObject,
		Properties: map[string]*genai.Schema{"summary": {Type: genai.TypeString}},
		Required:   []string{"summary"},
	}
}

func schemadTaskAgent(t *testing.T, name string, m *countingModel, schema *genai.Schema) adkagent.Agent {
	t.Helper()
	a, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name: name, Description: name, Instruction: "do the work",
		Model:        m,
		OutputSchema: schema,
	})
	if err != nil {
		t.Fatalf("NewTaskAgent(%q): %v", name, err)
	}
	return a
}

// boundedRun is runWith with a ceiling on how long it is willing to
// watch. The bound is the assertion, not a convenience: before the fix
// this run does not fail, it spins, and a test that only checked the
// answer would hang instead of reporting.
func boundedRun(t *testing.T, root adkagent.Agent, gate mastagent.CallGate, budget int) []*session.Event {
	t.Helper()
	r, err := runner.New(runner.Config{
		AppName:           "refusal-report-test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	ctx := mastagent.WithCallGate(t.Context(), gate)
	var out []*session.Event
	for ev, err := range r.Run(ctx, "user", "s1",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		out = append(out, ev)
		if len(out) > budget {
			t.Fatalf("the refusal did not end the run: still going after %d events, %d of them finish_task calls; "+
				"an invalid report is a retry instruction, and retrying a refused agent costs nothing",
				len(out), finishCalls(out))
		}
	}
	return out
}

// TestASchemadSpecialistsRefusalDoesNotLoop asserts both halves, because
// either alone passes on broken code: the refusal reaches the transcript
// as text rather than as a finish_task call mast cannot fill honestly,
// and the run ends.
func TestASchemadSpecialistsRefusalDoesNotLoop(t *testing.T) {
	coordModel := &countingModel{name: "coord", delegate: "sp"}
	spModel := &countingModel{name: "sp"}
	root := coordinator(t, "root", coordModel, schemadTaskAgent(t, "sp", spModel, findingSchema()))
	gate := &stubGate{refuse: map[string]string{"sp": "specialist cap"}}

	// Twenty is far more than the handful a terminating refusal needs
	// and far fewer than a free loop reaches in a millisecond.
	events := boundedRun(t, root, gate, 20)

	if got := spModel.count(); got != 0 {
		t.Errorf("the refused specialist called its model %d time(s), want 0", got)
	}
	if got := finishCalls(events); got != 0 {
		t.Errorf("the refusal was submitted as %d finish_task call(s); the specialist's schema requires %v, "+
			"and mast must not invent a finding to satisfy it", got, findingSchema().Required)
	}
	if text := transcript(events); !strings.Contains(text, mastagent.RefusalMarker) {
		t.Errorf("the refusal is not in the transcript an operator reads:\n%s", text)
	}
	if got := coordModel.count(); got == 0 {
		t.Error("the coordinator made no calls; a specialist's refusal must not stop the root")
	}
}

// TestAnUnschemadSpecialistStillReportsThroughFinishTask is the other
// half of the pair: the fallback must be reached only where it is
// needed. An agent whose finish_task takes ADK's own {"result": string}
// has a valid report shape, so it keeps reporting — that report is what
// lets a coordinator route around the spent path, and losing it here
// would trade one silent failure for another.
func TestAnUnschemadSpecialistStillReportsThroughFinishTask(t *testing.T) {
	coordModel := &countingModel{name: "coord", delegate: "sp"}
	root := coordinator(t, "root", coordModel, taskAgent(t, "sp", &countingModel{name: "sp"}))
	gate := &stubGate{refuse: map[string]string{"sp": "specialist cap"}}

	events := boundedRun(t, root, gate, 20)

	if got := finishCalls(events); got != 1 {
		t.Errorf("a refused specialist with no output schema made %d finish_task calls, want 1: "+
			"the report is how the caller learns the path is spent", got)
	}
	if text := transcript(events); !strings.Contains(text, mastagent.RefusalMarker) {
		t.Errorf("the refusal is not in the report:\n%s", text)
	}
}

func finishCalls(events []*session.Event) int {
	var n int
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == mastagent.FinishTaskToolName {
				n++
			}
		}
	}
	return n
}
