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

package compose

import (
	"context"
	"iter"
	"slices"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/workload"
)

// offeredToolsModel records the tool names offered on each round and
// otherwise behaves like dispatchOnceModel: dispatch once from the
// planner, finish from the specialist.
type offeredToolsModel struct {
	mu       sync.Mutex
	rounds   int
	planner  []string // tools offered on the planner's own turns
	subagent []string // tools offered on a dispatched specialist's turns
}

func (m *offeredToolsModel) Name() string { return "offered-tools" }

func (m *offeredToolsModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	offered := make([]string, 0, len(req.Tools))
	for name := range req.Tools {
		offered = append(offered, name)
	}
	slices.Sort(offered)

	_, canDispatch := req.Tools[planner.ToolInvokeSpecialist]
	m.mu.Lock()
	m.rounds++
	first := m.rounds == 1
	if canDispatch {
		m.planner = offered
	} else {
		m.subagent = offered
	}
	m.mu.Unlock()

	part := genai.NewPartFromFunctionCall("finish_task", map[string]any{"result": "done"})
	if canDispatch && first {
		part = genai.NewPartFromFunctionCall(planner.ToolInvokeSpecialist,
			map[string]any{"name": "alpha", "input": "look"})
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil)
	}
}

// RootConfig.SpecialistTools has to reach a Task specialist's tool
// declaration, and the way it is delivered is the whole point: a
// toolset would have been matched to a `tools.mcp: - server:` allowlist
// entry by name and dropped when there is no match, so the roster below
// — which declares deny-all `mcp: []`, the tightest posture mast
// documents — is exactly the one that would silently lose the escape
// hatch. It must still be offered the tool (#221).
//
// The planner root must NOT be offered it: retrieve_raw answers for a
// digested MCP response, and the planner never makes one.
func TestSpecialistToolsReachTaskSpecialistsAndNotTheRoot(t *testing.T) {
	probe, err := functiontool.New(
		functiontool.Config{Name: "retrieve_raw_probe", Description: "probe"},
		func(_ adkagent.Context, _ struct{}) (string, error) { return "", nil })
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	m := &offeredToolsModel{}
	root, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle: workload.Bundle{
			Name:        "triage",
			ToolCatalog: readOnlyCatalog(),
			Planner:     workload.Planner{Enabled: true},
		},
		Specs:           plannerSpecs(),
		Model:           m,
		ModelName:       "echo",
		SpecialistTools: []tool.Tool{probe},
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           "compose_test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	for _, err := range r.Run(context.Background(), "op", "outer-1",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
	}

	m.mu.Lock()
	plannerTools, subTools := m.planner, m.subagent
	m.mu.Unlock()

	if len(subTools) == 0 {
		t.Fatal("no specialist turn ran; the assertions below would pass vacuously")
	}
	if !slices.Contains(subTools, "retrieve_raw_probe") {
		t.Errorf("specialist was offered %v; RootConfig.SpecialistTools did not reach it", subTools)
	}
	if slices.Contains(plannerTools, "retrieve_raw_probe") {
		t.Errorf("the planner root was offered %v; SpecialistTools belong to specialists", plannerTools)
	}
}
