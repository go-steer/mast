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
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/planner"
)

// The dispatch tool's own token total is floored where it reads the
// provider's usage metadata, for the same reason pkg/budget's is
// (#332): sub_total_tokens is handed to the planner model as the price
// of a dispatch, and a credit tells it the cheapest specialist is the
// one whose provider miscounted.
//
// Pre-fix: sub_total_tokens = -480.
func TestASubRunsNegativeTokenCountIsNotACredit(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: func(req *model.LLMRequest) *model.LLMResponse {
		resp := specialistScript(req)
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     -400,
			CandidatesTokenCount: -80,
			TotalTokenCount:      -480,
		}
		return resp
	}}
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
	)

	root, err := planner.NewRoot(planner.Config{
		Name:        "w",
		Model:       plModel,
		Specialists: map[string]adkagent.Agent{"sp": buildSpecialist(t, "sp", spModel)},
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())
	events := runPlanner(t, r, "outer-negative-usage", genai.NewContentFromText("work", genai.RoleUser))

	res := dispatchResult(t, events)
	// The tool result round-trips as JSON on its way to the model, so
	// the counts arrive as float64 however they were written.
	if tokens := number(t, res, "sub_total_tokens"); tokens != 0 {
		t.Errorf("sub_total_tokens = %v, want 0 — a miscounted call costs nothing, never less than nothing", tokens)
	}
	// The call still happened, and the planner is still told so: the
	// floor drops an impossible count, not the event that carried it.
	if calls := number(t, res, "sub_model_calls"); calls != 1 {
		t.Errorf("sub_model_calls = %v, want 1", calls)
	}
}

// number reads one numeric field of a tool result, whichever numeric
// type it survived the round trip as.
func number(t *testing.T, res map[string]any, key string) float64 {
	t.Helper()
	switch v := res[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	default:
		t.Fatalf("%s = %v (%T), want a number", key, res[key], res[key])
		return 0
	}
}
