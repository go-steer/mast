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

// The planner tells a model which specialists it may dispatch to in
// PROSE — invoke_specialist takes a name as an argument, so unlike a
// coordinator's roster there is no per-specialist tool declaration to
// read it off. mast's offline UAT double (pkg/agent's toolactor) has to
// find the roster somewhere, and the only place it exists is that
// sentence.
//
// So the sentence is a contract with exactly one other party, and this
// file is the whole of its enforcement. The same arrangement pkg/graph
// has with ApprovedCallsMarker, for the same reason: a phrase that drifts
// does not fail loudly. The fake would find no roster, dispatch nothing,
// finish the workload, and every planner leg in scripts/uat-v0.6.sh would
// stay green while measuring an empty planner.

package planner_test

import (
	"strings"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/planner"
)

// The tool name the fake calls is the tool name the planner declares.
func TestPlannerDispatchToolNameIsShared(t *testing.T) {
	if planner.ToolInvokeSpecialist != mastagent.PlannerDispatchTool {
		t.Fatalf("planner declares %q, the offline double calls %q",
			planner.ToolInvokeSpecialist, mastagent.PlannerDispatchTool)
	}
}

// And the roster phrase. Asserted against the RENDERED instruction rather
// than against the template, because rendering is where the roster and
// the surrounding punctuation actually meet — a template that still
// carries the phrase can still emit a roster the parser reads wrong.
func TestPlannerInstructionNamesTheRoster(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	sp := map[string]adkagent.Agent{
		"diag":     buildSpecialist(t, "diag", spModel),
		"remediar": buildSpecialist(t, "remediar", spModel),
	}

	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel)
	root, err := planner.NewRoot(planner.Config{Name: "gke-triage", Model: plModel, Specialists: sp})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	runPlanner(t, newRunner(t, root, session.InMemoryService()), "s1",
		genai.NewContentFromText("work", genai.RoleUser))
	sys := systemText(t, plModel)

	if !strings.Contains(sys, mastagent.RosterPreamble) {
		t.Fatalf("the planner instruction no longer contains %q, which is the only thing\n"+
			"pkg/agent's offline double has to find a roster by. Update\n"+
			"mastagent.RosterPreamble in the same change; a silent drift here\n"+
			"turns every planner UAT leg into a test of an empty roster.\nGot:\n%s",
			mastagent.RosterPreamble, sys)
	}

	// The round trip: the real rendered instruction, handed to the real
	// parser, yields the real roster. Contains() alone would pass on a
	// template that named the roster in a shape the parser cannot split.
	m := mastagent.NewToolActorModel("test")
	req := &model.LLMRequest{
		Tools: map[string]any{planner.ToolInvokeSpecialist: nil, "finish_task": nil},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(sys, genai.RoleUser),
		},
		Contents: []*genai.Content{genai.NewContentFromText(`{"reason":"ReadStatus"}`, genai.RoleUser)},
	}
	var got *genai.FunctionCall
	for resp, err := range m.GenerateContent(t.Context(), req, false) {
		if err != nil {
			t.Fatalf("toolactor: %v", err)
		}
		if resp != nil && resp.Content != nil {
			for _, p := range resp.Content.Parts {
				if p != nil && p.FunctionCall != nil && got == nil {
					got = p.FunctionCall
				}
			}
		}
	}
	if got == nil || got.Name != planner.ToolInvokeSpecialist {
		t.Fatalf("the double answered a planner turn with %+v, want a dispatch: "+
			"it read no roster out of the instruction, so it went straight to finish_task", got)
	}
	// Specialists are declaration-ordered by NewRoot; "diag" sorts first
	// either way, so this pins the parse rather than the ordering.
	if got.Args["name"] != "diag" {
		t.Errorf("dispatched to %v, want diag — the roster parsed as %v",
			got.Args["name"], got.Args)
	}
}
