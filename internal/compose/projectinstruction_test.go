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
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/internal/planner"
	"github.com/go-steer/mast/pkg/workload"
)

const clusterFact = "This agent serves projects/p/locations/l/clusters/c."

// systemPromptModel records the system instruction of the root's turns
// and of a dispatched specialist's turns. Under the planner it
// dispatches "alpha" once; everywhere else it answers in text.
type systemPromptModel struct {
	mu         sync.Mutex
	rounds     int
	root       string
	specialist string
}

func (m *systemPromptModel) Name() string { return "system-prompt" }

func (m *systemPromptModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	var sys strings.Builder
	if req.Config != nil && req.Config.SystemInstruction != nil {
		for _, p := range req.Config.SystemInstruction.Parts {
			sys.WriteString(p.Text)
		}
	}
	_, isPlanner := req.Tools[planner.ToolInvokeSpecialist]
	m.mu.Lock()
	m.rounds++
	first := m.rounds == 1
	if first {
		m.root = sys.String()
	} else if !isPlanner {
		m.specialist = sys.String()
	}
	m.mu.Unlock()

	part := genai.NewPartFromText("done")
	switch {
	case isPlanner && first:
		part = genai.NewPartFromFunctionCall(planner.ToolInvokeSpecialist,
			map[string]any{"name": "alpha", "input": "look"})
	case !first:
		part = genai.NewPartFromFunctionCall("finish_task", map[string]any{"result": "done"})
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil)
	}
}

func runOnce(t *testing.T, root adkagent.Agent) {
	t.Helper()
	r, err := runner.New(runner.Config{
		AppName:           "compose_test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	for _, err := range r.Run(context.Background(), "op", "s-1",
		genai.NewContentFromText("incident", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
	}
}

// The specialist doing the work is the one that has to know which
// cluster it is looking at, so the project instruction rides on every
// spec, ahead of the spec's own prompt. The caller's specs are left as
// they were: they are also what the daemon reports and hashes.
func TestProjectInstructionReachesSpecialists(t *testing.T) {
	specs := plannerSpecs()
	m := &systemPromptModel{}
	root, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle: workload.Bundle{
			Name:        "triage",
			ToolCatalog: readOnlyCatalog(),
			Planner:     workload.Planner{Enabled: true},
		},
		Specs:              specs,
		ProjectInstruction: clusterFact,
		Model:              m,
		ModelName:          "echo",
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	runOnce(t, root)

	m.mu.Lock()
	got := m.specialist
	m.mu.Unlock()
	if got == "" {
		t.Fatal("no specialist turn ran; the assertions below would pass vacuously")
	}
	fact, own := strings.Index(got, clusterFact), strings.Index(got, "look")
	if fact < 0 || own < 0 || fact > own {
		t.Errorf("specialist system prompt = %q; want the project instruction ahead of the spec's own", got)
	}
	if specs[0].Instruction != "look" {
		t.Errorf("caller's spec instruction became %q; BuildRoot must work on a copy", specs[0].Instruction)
	}
}

// The coordinator gets it too, ahead of its bundle-derived prompt: it
// is the agent that summarises for the operator, and it should name the
// same cluster the specialist looked at.
func TestProjectInstructionReachesCoordinator(t *testing.T) {
	m := &systemPromptModel{}
	root, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle: workload.Bundle{
			Name:        "triage",
			ToolCatalog: readOnlyCatalog(),
			Specialists: []string{"alpha"},
		},
		Specs:              plannerSpecs(),
		ProjectInstruction: clusterFact,
		Model:              m,
		ModelName:          "echo",
		Dispatch:           DispatchCoordinator,
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	runOnce(t, root)

	m.mu.Lock()
	got := m.root
	m.mu.Unlock()
	fact, own := strings.Index(got, clusterFact), strings.Index(got, "coordinator for the")
	if fact < 0 || own < 0 || fact > own {
		t.Errorf("coordinator system prompt = %q; want the project instruction ahead of the coordinator's own", got)
	}
}
