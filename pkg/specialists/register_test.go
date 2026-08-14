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
	"errors"
	"iter"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
)

// countingModel is a model.LLM that records how many times it was
// called. The model an agent was built with is not readable back off an
// adkagent.Agent, so the only honest way to assert "this specialist runs
// on that tier" is to run it and see which model answered.
type countingModel struct {
	name  string
	calls int
	// decls is the tool surface of the last request. ADK assembles it
	// several layers below the config a Spec turns into, so this is the
	// only place it can be read.
	decls []string
}

func (m *countingModel) Name() string { return m.name }

func (m *countingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.calls++
		m.decls = nil
		if req.Config != nil {
			for _, gt := range req.Config.Tools {
				for _, fd := range gt.FunctionDeclarations {
					m.decls = append(m.decls, fd.Name)
				}
			}
		}
		resp := &model.LLMResponse{
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}
		// Task-mode agents auto-install finish_task and only terminate
		// by calling it; play along so the run completes.
		if _, ok := req.Tools["finish_task"]; ok {
			resp.Content = &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall("finish_task", map[string]any{
					"result": m.name,
				})},
			}
		} else {
			resp.Content = genai.NewContentFromText(m.name, genai.RoleModel)
		}
		yield(resp, nil)
	}
}

// delegatingModel drives a Chat-mode coordinator through one delegation
// per named sub-agent, then stops. Task- and SingleTurn-mode agents
// cannot be a runner root, so reaching a specialist's own model means
// going through a coordinator that transfers to it.
type delegatingModel struct {
	order []string
}

func (m *delegatingModel) Name() string { return "coordinator" }

func (m *delegatingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		done := 0
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			for _, p := range c.Parts {
				if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name != "finish_task" {
					done++
				}
			}
		}
		resp := &model.LLMResponse{TurnComplete: true, FinishReason: genai.FinishReasonStop}
		if done < len(m.order) {
			resp.Content = &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall(m.order[done], map[string]any{"request": "go"})},
			}
		} else {
			resp.Content = genai.NewContentFromText("done", genai.RoleModel)
		}
		yield(resp, nil)
	}
}

// runUnderCoordinator drives one user turn through a Chat coordinator
// that delegates to every one of subs in turn, so each specialist's own
// model receives a call.
func runUnderCoordinator(t *testing.T, subs []adkagent.Agent) {
	t.Helper()
	order := make([]string, 0, len(subs))
	for _, s := range subs {
		order = append(order, s.Name())
	}
	root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        "coord",
		Description: "Coordinator under test.",
		Instruction: "Delegate.",
		Model:       &delegatingModel{order: order},
		SubAgents:   subs,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           "register-test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	msg := genai.NewContentFromText("hello", genai.RoleUser)
	for _, err := range r.Run(context.Background(), "user", "s1", msg, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
	}
}

// TestBuild_HonorsSpecModel is W1.1's exit criterion: a roster that
// declares a cheap-tier analyst and a frontier-tier synthesizer builds
// two agents that actually run on those two models, while a specialist
// with no override still runs on the parent's.
func TestBuild_HonorsSpecModel(t *testing.T) {
	parent := &countingModel{name: "parent"}
	analyst := &countingModel{name: "analyst-tier"}
	synth := &countingModel{name: "synth-tier"}

	byName := map[string]*countingModel{
		"analyst-tier": analyst,
		"synth-tier":   synth,
	}
	var asked []string
	resolve := func(name string) (model.LLM, error) {
		asked = append(asked, name)
		m, ok := byName[name]
		if !ok {
			return nil, errors.New("unknown model " + name)
		}
		return m, nil
	}

	specs := []specialists.Spec{
		{Name: "pod_inspector", Description: "d", Instruction: "i", Mode: specialists.ModeTask, Model: "analyst-tier"},
		{Name: "synthesizer", Description: "d", Instruction: "i", Mode: specialists.ModeSingleTurn, Model: "synth-tier"},
		{Name: "inheritor", Description: "d", Instruction: "i", Mode: specialists.ModeTask},
	}
	agents, err := specialists.BuildAll(specs, specialists.BuildOptions{Model: parent, Resolve: resolve})
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	if got, want := len(asked), 2; got != want {
		t.Errorf("resolver called %d times %v, want %d (the inheritor must not resolve)", got, asked, want)
	}

	runUnderCoordinator(t, agents)
	if analyst.calls == 0 {
		t.Error("analyst-tier model was never called: pod_inspector did not run on its declared tier")
	}
	if synth.calls == 0 {
		t.Error("synth-tier model was never called: synthesizer did not run on its declared tier")
	}
	if parent.calls == 0 {
		t.Error("parent model was never called: the specialist with no override did not inherit it")
	}
}

// TestBuild_OverrideWithoutResolver pins the refusal: dropping a
// declared override on the floor is the bug W1.1 fixes, so an override
// that cannot be resolved must fail the build rather than quietly
// inherit the parent's model.
func TestBuild_OverrideWithoutResolver(t *testing.T) {
	spec := specialists.Spec{Name: "analyst", Description: "d", Instruction: "i", Model: "some-tier"}
	_, err := specialists.Build(spec, specialists.BuildOptions{Model: &countingModel{name: "parent"}})
	if err == nil {
		t.Fatal("expected an error for a declared override with no resolver, got nil")
	}
}

func TestBuild_ResolverFailures(t *testing.T) {
	parent := &countingModel{name: "parent"}
	sentinel := errors.New("no credentials")

	tests := []struct {
		name    string
		resolve specialists.ModelResolver
	}{
		{"resolver error", func(string) (model.LLM, error) { return nil, sentinel }},
		{"resolver returns nil", func(string) (model.LLM, error) { return nil, nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := specialists.Spec{Name: "analyst", Description: "d", Instruction: "i", Model: "some-tier"}
			_, err := specialists.Build(spec, specialists.BuildOptions{Model: parent, Resolve: tc.resolve})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// TestBuild_ResolverUnusedWithoutOverride guards the inheritance path:
// a roster with no overrides must never touch the resolver, so wiring
// one in cannot change behavior for existing bundles.
func TestBuild_ResolverUnusedWithoutOverride(t *testing.T) {
	called := false
	resolve := func(string) (model.LLM, error) {
		called = true
		return nil, errors.New("must not be called")
	}
	spec := specialists.Spec{Name: "plain", Description: "d", Instruction: "i"}
	if _, err := specialists.Build(spec, specialists.BuildOptions{Model: &countingModel{name: "parent"}, Resolve: resolve}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if called {
		t.Error("resolver called for a spec with no model override")
	}
}

// A Task specialist must not be offered transfer_to_agent. Under a Chat
// coordinator — the topology pkg/router builds — the coordinator is its only
// possible destination, since ADK's transferTargets skips Task-mode peers,
// and taking it aborts the run: the transfer is forwarded in-process, so the
// coordinator's runChat executes under the specialist's node context and
// re-dispatches the unresolved delegation through workflow.RunNode, which
// fails with "RunNode called outside a dynamic node".
//
// It is also right on its own terms. A specialist reports through
// finish_task; one that transfers abandons the question it was asked and the
// coordinator gets nothing to merge.
func TestBuild_TaskSpecialistsCannotTransfer(t *testing.T) {
	sub := &countingModel{name: "sub"}
	spec := specialists.Spec{Name: "pod_inspector", Description: "d", Instruction: "i", Mode: specialists.ModeTask}
	a, err := specialists.Build(spec, specialists.BuildOptions{Model: sub})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	runUnderCoordinator(t, []adkagent.Agent{a})

	if sub.calls == 0 {
		t.Fatal("the specialist's model was never called, so this test proves nothing")
	}
	var sawFinish bool
	for _, name := range sub.decls {
		if name == "transfer_to_agent" {
			t.Errorf("the specialist is offered transfer_to_agent; its only target is the "+
				"coordinator and taking it aborts the run. Declared: %v", sub.decls)
		}
		if name == "finish_task" {
			sawFinish = true
		}
	}
	// Removing the escape hatch must not remove the exit: a specialist with
	// no finish_task closes no delegation at all.
	if !sawFinish {
		t.Errorf("the specialist has no finish_task, so it cannot report; declared: %v", sub.decls)
	}
}
