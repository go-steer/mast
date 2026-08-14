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

package specialists_test

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
)

// askingModel is a specialist that ends its turn with a question instead of a
// report — the failure FinishOnStall exists for. It never calls finish_task.
type askingModel struct {
	name  string
	calls int
}

func (m *askingModel) Name() string { return m.name }

func (m *askingModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.calls++
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText("Could you run kubectl get all and paste the output?", genai.RoleModel),
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil)
	}
}

const stallQuestion = "kubectl get all"

// TestStallGuard_Unguarded is the control, and without it every assertion
// below could pass against a runtime that closes the delegation on its own.
//
// A Task sub-agent that drains its iterator without calling finish_task ends
// the *caller's* turn too. The coordinator never regains control, so the
// delegation it is waiting on is never answered and the whole run produces
// nothing — not a bad answer, no answer.
func TestStallGuard_Unguarded(t *testing.T) {
	m := &askingModel{name: "asker"}
	spec := specialists.Spec{Name: "inspector", Description: "d", Instruction: "i", Mode: specialists.ModeTask}
	agent, err := specialists.Build(spec, specialists.BuildOptions{Model: m})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	events, err := runSpecialist(t, agent)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if m.calls == 0 {
		t.Fatal("the specialist never ran, so this control proves nothing")
	}
	if resps := responsesTo(events, "inspector"); len(resps) != 0 {
		t.Fatalf("the delegation closed by itself, so the guard below is being credited "+
			"with something ADK now does: %v", resps)
	}
}

// TestStallGuard_ClosesTheDelegation is the treatment: the same specialist,
// the same question, one field set.
func TestStallGuard_ClosesTheDelegation(t *testing.T) {
	m := &askingModel{name: "asker"}
	spec := specialists.Spec{Name: "inspector", Description: "d", Instruction: "i", Mode: specialists.ModeTask}
	agent, err := specialists.Build(spec, specialists.BuildOptions{
		Model:   m,
		OnStall: func(specialists.Spec) mastagent.StallPayload { return nil },
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	events, err := runSpecialist(t, agent)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	resps := responsesTo(events, "inspector")
	if len(resps) != 1 {
		t.Fatalf("got %d delegation responses, want 1: the coordinator never regained control", len(resps))
	}
	if e, ok := resps[0]["error"]; ok {
		t.Fatalf("the synthesised finish_task call was refused: %v", e)
	}
	result, ok := resps[0]["result"].(string)
	if !ok {
		t.Fatalf("result is %T, want the default payload's string", resps[0]["result"])
	}

	// Countable, so that a rescued run is not filed next to a complete one.
	if !mastagent.Stalled(result) {
		t.Errorf("the result is not recognisable as a stall: %q", result)
	}
	// And informative. The question the specialist wanted to ask names the data
	// it could not get, which is usually the most useful thing in the whole
	// delegation — it is how a missing read-path tool gets found.
	if !strings.Contains(result, stallQuestion) {
		t.Errorf("the specialist's last words were dropped: %q", result)
	}
}

// TestStallGuard_LeavesAWorkingSpecialistAlone is the neutralize half. The
// guard fires on a *terminal silent* turn, and a specialist that reports
// normally must reach the coordinator with its own result — otherwise the test
// above is passing because every delegation now returns a stall report.
func TestStallGuard_LeavesAWorkingSpecialistAlone(t *testing.T) {
	m := &scriptedTaskModel{name: "analyst", args: []map[string]any{{"result": "all clear"}}}
	spec := specialists.Spec{Name: "worker", Description: "d", Instruction: "i", Mode: specialists.ModeTask}
	agent, err := specialists.Build(spec, specialists.BuildOptions{
		Model:   m,
		OnStall: func(specialists.Spec) mastagent.StallPayload { return nil },
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	events, err := runSpecialist(t, agent)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	resps := responsesTo(events, "worker")
	if len(resps) != 1 {
		t.Fatalf("got %d delegation responses, want 1: %v", len(resps), resps)
	}
	if got, _ := resps[0]["result"].(string); got != "all clear" {
		t.Errorf("result = %v, want the specialist's own report", resps[0]["result"])
	}
	if got, _ := resps[0]["result"].(string); mastagent.Stalled(got) {
		t.Error("a specialist that reported was reported as stalled")
	}
}

// TestStallGuard_HonorsTheSpecsOutputSchema is the case the per-spec seam
// exists for. The payload is the roster's, and the runtime validates the
// injected call against the schema exactly as it validates a model-issued one:
// mast substituting its own shape here would produce a refusal, which is the
// unresolved delegation the guard was installed to prevent.
func TestStallGuard_HonorsTheSpecsOutputSchema(t *testing.T) {
	m := &askingModel{name: "asker"}
	spec := specialists.Spec{
		Name:         "auditor",
		Description:  "d",
		Instruction:  "i",
		Mode:         specialists.ModeTask,
		OutputSchema: reportSchema(),
	}
	agent, err := specialists.Build(spec, specialists.BuildOptions{
		Model: m,
		OnStall: func(s specialists.Spec) mastagent.StallPayload {
			return func(agentName, lastWords string) map[string]any {
				return map[string]any{
					"summary":  mastagent.StallText(agentName, lastWords),
					"severity": "OK",
				}
			}
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	events, err := runSpecialist(t, agent)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	resps := responsesTo(events, "auditor")
	if len(resps) != 1 {
		t.Fatalf("got %d delegation responses, want 1: a payload the output schema refused "+
			"would leave the delegation open, since this specialist never retries", len(resps))
	}
	if e, ok := resps[0]["error"]; ok {
		t.Fatalf("the synthesised report was refused by the output schema: %v", e)
	}
	// A schema'd Task specialist's delegation response is the validated object
	// itself. The "result" key seen in the default case is that schema's own
	// single property, not a wrapper ADK adds.
	if _, wrapped := resps[0]["result"]; wrapped {
		t.Errorf("the response is wrapped under \"result\": %v", resps[0])
	}
	if got := resps[0]["severity"]; got != "OK" {
		t.Errorf("severity = %v, want the payload's own value", got)
	}
	summary, _ := resps[0]["summary"].(string)
	if !mastagent.Stalled(summary) {
		t.Errorf("summary is not recognisable as a stall: %q", summary)
	}
	if !strings.Contains(summary, stallQuestion) {
		t.Errorf("the specialist's last words were dropped: %q", summary)
	}
}

// TestStallGuard_RefusesADefaultPayloadAgainstASchema pins the build error.
//
// The default payload is `{"result": <string>}`, which no spec-declared
// output_schema is going to accept. Falling back to it would swap a build
// failure for a run-time one, and specifically for a refused finish_task call
// on the one turn nobody is watching — the exact failure the guard exists to
// prevent, reintroduced by the guard.
func TestStallGuard_RefusesADefaultPayloadAgainstASchema(t *testing.T) {
	spec := specialists.Spec{
		Name:         "auditor",
		Description:  "d",
		Instruction:  "i",
		Mode:         specialists.ModeTask,
		OutputSchema: reportSchema(),
	}
	_, err := specialists.Build(spec, specialists.BuildOptions{
		Model:   &askingModel{name: "asker"},
		OnStall: func(specialists.Spec) mastagent.StallPayload { return nil },
	})
	if err == nil {
		t.Fatal("Build accepted a stall guard with no payload for a spec that declares an output_schema")
	}
	if !strings.Contains(err.Error(), "auditor") {
		t.Errorf("the error does not name the offending spec: %v", err)
	}
}

// And the opt-in is real: a roster that never sets OnStall gets no guard, no
// callback and no behaviour change. This is what makes the field a choice
// rather than a policy — see the note on BuildOptions.OnStall for why
// fabricating a tool call is not defaulted on the way the transfer flags are.
func TestStallGuard_IsOptIn(t *testing.T) {
	m := &askingModel{name: "asker"}
	spec := specialists.Spec{Name: "quiet", Description: "d", Instruction: "i", Mode: specialists.ModeTask}
	agent, err := specialists.Build(spec, specialists.BuildOptions{Model: m})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	events, err := runSpecialist(t, agent)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resps := responsesTo(events, "quiet"); len(resps) != 0 {
		t.Errorf("a roster that did not ask for the guard got one: %v", resps)
	}
}
