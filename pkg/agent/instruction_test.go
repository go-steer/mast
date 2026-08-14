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

package agent_test

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

// recordingModel is a scripted model.LLM that captures the system
// instruction and the tool declarations of every request it receives, so
// tests can observe what the constructors actually wired into the agent —
// neither is readable back off an adkagent.Agent, but both are visible to
// the model on every call.
type recordingModel struct {
	name    string
	respond func(req *model.LLMRequest) *model.LLMResponse
	systems []string
	decls   []*genai.FunctionDeclaration
}

func (m *recordingModel) Name() string { return m.name }

func (m *recordingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.systems = append(m.systems, systemText(req))
		m.decls = append(m.decls, declarations(req)...)
		yield(m.respond(req), nil)
	}
}

// declarations flattens the function declarations attached to a request.
// ADK assembles these several layers below llmagent.Config — the transfer
// tool, for one, is appended by a request processor — so the request is the
// only place the model's actual surface can be read.
func declarations(req *model.LLMRequest) []*genai.FunctionDeclaration {
	if req == nil || req.Config == nil {
		return nil
	}
	var out []*genai.FunctionDeclaration
	for _, t := range req.Config.Tools {
		out = append(out, t.FunctionDeclarations...)
	}
	return out
}

func systemText(req *model.LLMRequest) string {
	if req == nil || req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		if p != nil {
			b.WriteString(p.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func textResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      genai.NewContentFromText(text, genai.RoleModel),
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
}

func callResponse(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
		},
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
}

// coordinatorScript delegates to each named sub-agent tool in order —
// one per round, keyed off how many delegations have already returned
// — then finishes with a plain text reply.
func coordinatorScript(order ...string) func(*model.LLMRequest) *model.LLMResponse {
	return func(req *model.LLMRequest) *model.LLMResponse {
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
		if done < len(order) {
			return callResponse(order[done], map[string]any{"request": "go"})
		}
		return textResponse("all delegations complete")
	}
}

// specialistScript completes a Task-mode delegation via finish_task
// when that helper is installed, and otherwise (SingleTurn) replies
// with plain text.
func specialistScript(req *model.LLMRequest) *model.LLMResponse {
	if _, ok := req.Tools["finish_task"]; ok {
		return callResponse("finish_task", map[string]any{"result": "done"})
	}
	return textResponse("category-a")
}

// buildTree wires coordinator -> {task, single-turn} agents around the
// given instructions ("" exercises the per-mode fallback) and returns
// the three recording models.
func buildTree(t *testing.T, coordInstr, taskInstr, stInstr string) (root adkagent.Agent, coordModel, taskModel, stModel *recordingModel) {
	t.Helper()

	taskModel = &recordingModel{name: "rec-task", respond: specialistScript}
	stModel = &recordingModel{name: "rec-st", respond: specialistScript}
	coordModel = &recordingModel{name: "rec-coord", respond: coordinatorScript("task_sub", "st_sub")}

	taskAgent, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        "task_sub",
		Description: "Task-mode specialist under test.",
		Instruction: taskInstr,
		Model:       taskModel,
	})
	if err != nil {
		t.Fatalf("NewTaskAgent: %v", err)
	}
	stAgent, err := mastagent.NewSingleTurnAgent(mastagent.SingleTurnAgentConfig{
		Name:        "st_sub",
		Description: "SingleTurn classifier under test.",
		Instruction: stInstr,
		Model:       stModel,
	})
	if err != nil {
		t.Fatalf("NewSingleTurnAgent: %v", err)
	}
	root, err = mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        "coord",
		Description: "Coordinator under test.",
		Instruction: coordInstr,
		Model:       coordModel,
		SubAgents:   []adkagent.Agent{taskAgent, stAgent},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	return root, coordModel, taskModel, stModel
}

// runTree drives one user turn through a runner so every agent in the
// tree receives a model call.
func runTree(t *testing.T, root adkagent.Agent) {
	t.Helper()
	r, err := runner.New(runner.Config{
		AppName:           "instruction-test",
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

// captured flattens a recording model's observed system instructions
// and fails the test if the model was never called.
func captured(t *testing.T, m *recordingModel) string {
	t.Helper()
	if len(m.systems) == 0 {
		t.Fatalf("model %q was never called", m.name)
	}
	return strings.Join(m.systems, "\n---\n")
}

func TestDefaultInstructionsNonEmptyAndDistinct(t *testing.T) {
	defaults := map[string]string{
		"chat":        mastagent.DefaultChatInstruction,
		"task":        mastagent.DefaultTaskInstruction,
		"single_turn": mastagent.DefaultSingleTurnInstruction,
	}
	for mode, text := range defaults {
		if strings.TrimSpace(text) == "" {
			t.Errorf("default %s instruction is empty", mode)
		}
	}
	if defaults["chat"] == defaults["task"] ||
		defaults["chat"] == defaults["single_turn"] ||
		defaults["task"] == defaults["single_turn"] {
		t.Error("per-mode default instructions must be pairwise distinct")
	}
	// Spot-check that each variant carries its mode's framing
	// (positioning.md "Change shape" -> DefaultInstruction).
	if !strings.Contains(defaults["chat"], "operator") {
		t.Error("chat default should frame an operator-facing conversation")
	}
	if !strings.Contains(defaults["task"], "unattended") || !strings.Contains(defaults["task"], "eventlog") {
		t.Error("task default should carry the unattended-loop discipline")
	}
	if len(defaults["single_turn"]) > len(defaults["task"]) {
		t.Error("single_turn default should be the minimal variant")
	}
}

func TestModeDefaultAppliedWhenInstructionEmpty(t *testing.T) {
	root, coordModel, taskModel, stModel := buildTree(t, "", "", "")
	runTree(t, root)

	if got := captured(t, coordModel); !strings.Contains(got, mastagent.DefaultChatInstruction) {
		t.Errorf("coordinator system instruction missing DefaultChatInstruction; got:\n%s", got)
	}
	if got := captured(t, taskModel); !strings.Contains(got, mastagent.DefaultTaskInstruction) {
		t.Errorf("task agent system instruction missing DefaultTaskInstruction; got:\n%s", got)
	}
	if got := captured(t, stModel); !strings.Contains(got, mastagent.DefaultSingleTurnInstruction) {
		t.Errorf("single-turn agent system instruction missing DefaultSingleTurnInstruction; got:\n%s", got)
	}
	// Each mode gets its own variant, not a shared blob.
	if got := captured(t, taskModel); strings.Contains(got, mastagent.DefaultChatInstruction) {
		t.Error("task agent should not receive the chat default")
	}
	if got := captured(t, stModel); strings.Contains(got, mastagent.DefaultTaskInstruction) {
		t.Error("single-turn agent should not receive the task default")
	}
}

func TestExplicitInstructionUsedVerbatim(t *testing.T) {
	const (
		coordInstr = "CUSTOM-COORD: route everything to task_sub then st_sub."
		taskInstr  = "CUSTOM-TASK: diagnose and finish."
		stInstr    = "CUSTOM-ST: reply with one word."
	)
	root, coordModel, taskModel, stModel := buildTree(t, coordInstr, taskInstr, stInstr)
	runTree(t, root)

	checks := []struct {
		model   *recordingModel
		want    string
		defText string
		mode    string
	}{
		{coordModel, coordInstr, mastagent.DefaultChatInstruction, "chat"},
		{taskModel, taskInstr, mastagent.DefaultTaskInstruction, "task"},
		{stModel, stInstr, mastagent.DefaultSingleTurnInstruction, "single_turn"},
	}
	for _, c := range checks {
		got := captured(t, c.model)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s agent: explicit instruction not passed through; got:\n%s", c.mode, got)
		}
		// Verbatim means the default is not prepended or appended.
		if strings.Contains(got, c.defText) {
			t.Errorf("%s agent: default instruction leaked alongside an explicit one", c.mode)
		}
	}
}
