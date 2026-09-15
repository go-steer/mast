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
	"strings"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/planner"
)

// dispatchThoughtCanary is the specialist's reasoning. If invoke_specialist
// returns it, it lands in the planner's own prompt on the next round AND in
// the session transcript as a tool result — a durable copy of a thinking
// block, in a record whose readers are operators rather than providers.
const dispatchThoughtCanary = "<reasoning> the on-call rotation password is hunter2"

// TestDispatchResultSkipsThinking pins the specialist result invoke_specialist
// hands back (#370).
//
// The shape is the one a Task specialist produces when it answers without
// calling finish_task: the sub-run has no Output, so the dispatcher falls back
// to the last model text. A specialist whose final turn is reasoning followed
// by prose is the ordinary case; the fallback must pick the prose.
//
// Neutralize check: restore `part.Text != ""` in dispatch.go's event loop and
// result is the reasoning.
func TestDispatchResultSkipsThinking(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: func(req *model.LLMRequest) *model.LLMResponse {
		return &model.LLMResponse{Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{Text: "the image tag is missing from the registry"},
				{Text: dispatchThoughtCanary, Thought: true, ThoughtSignature: []byte("sig-370")},
			},
		}}
	}}
	sp := buildSpecialist(t, "sp", spModel)

	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
	)
	root, err := planner.NewRoot(planner.Config{
		Name:        "w",
		Model:       plModel,
		Specialists: map[string]adkagent.Agent{"sp": sp},
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())
	runPlanner(t, r, "outer-thought", genai.NewContentFromText("work", genai.RoleUser))

	var results []map[string]any
	for _, req := range plModel.requests() {
		results = append(results, functionResponses(req)[planner.ToolInvokeSpecialist]...)
	}
	if len(results) == 0 {
		t.Fatal("the planner never saw an invoke_specialist response; this test would pass for the wrong reason")
	}
	got, _ := results[len(results)-1]["result"].(string)
	if strings.Contains(got, dispatchThoughtCanary) {
		t.Errorf("the specialist result carries a thinking block: %q", got)
	}
	if got != "the image tag is missing from the registry" {
		t.Errorf("specialist result = %q, want the visible text alone", got)
	}
}
