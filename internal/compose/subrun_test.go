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
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/workload"
)

// dispatchOnceModel calls invoke_specialist on its first round for the
// agent that has the tool, finishes on every later round, and answers a
// specialist's own round with finish_task. One model serves both sides
// because compose hands the root model down to specialists that declare
// no override — which is also what makes the sub-run's spend traceable
// to this fake.
type dispatchOnceModel struct {
	mu       sync.Mutex
	rounds   int
	subCalls int
}

func (m *dispatchOnceModel) Name() string { return "dispatch-once" }

func (m *dispatchOnceModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	_, canDispatch := req.Tools[planner.ToolInvokeSpecialist]
	m.mu.Lock()
	m.rounds++
	first := m.rounds == 1
	if !canDispatch {
		m.subCalls++
	}
	m.mu.Unlock()

	var part *genai.Part
	switch {
	case canDispatch && first:
		part = genai.NewPartFromFunctionCall(planner.ToolInvokeSpecialist,
			map[string]any{"name": "alpha", "input": "look"})
	default:
		part = genai.NewPartFromFunctionCall("finish_task", map[string]any{"result": "done"})
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     100,
				CandidatesTokenCount: 20,
				TotalTokenCount:      120,
			},
		}, nil)
	}
}

// countingSubRun is the host end of the seam. One value is both the
// observer and the sink it hands out — this test cares that the seam is
// threaded, not how a host scopes it.
type countingSubRun struct {
	mu       sync.Mutex
	events   int
	sessions []string
}

func (c *countingSubRun) SubRun(sessionID, _ string) planner.SubRunSink {
	return &countingSubRunSink{c: c, sessionID: sessionID}
}

type countingSubRunSink struct {
	c         *countingSubRun
	sessionID string
}

func (s *countingSubRunSink) Observe(*session.Event) error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.events++
	s.c.sessions = append(s.c.sessions, s.sessionID)
	return nil
}

func (s *countingSubRunSink) Close() {}

// RootConfig.SubRunObserver has to reach the planner's dispatch tool,
// not merely exist: a declared field that nothing threads is not a
// seam, and #226 is precisely a case of accounting that was assumed to
// be wired and was not. This runs a real dispatch through a composed
// planner root and asserts the host heard it.
func TestSubRunObserverReachesThePlannerDispatch(t *testing.T) {
	obs := &countingSubRun{}
	root, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle: workload.Bundle{
			Name:        "triage",
			ToolCatalog: readOnlyCatalog(),
			Planner:     workload.Planner{Enabled: true},
		},
		Specs:          plannerSpecs(),
		Model:          &dispatchOnceModel{},
		ModelName:      "echo",
		SubRunObserver: obs,
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

	obs.mu.Lock()
	events, sessions := obs.events, obs.sessions
	obs.mu.Unlock()
	if events == 0 {
		t.Fatal("the observer saw nothing; RootConfig.SubRunObserver is not reaching the dispatch tool")
	}
	for _, s := range sessions {
		if s != "outer-1" {
			t.Errorf("observed session %q, want the outer session", s)
		}
	}
}
