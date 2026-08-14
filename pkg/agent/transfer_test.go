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
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

const transferTool = "transfer_to_agent"

// buildSpecialist wires one Task-mode specialist under a Chat-mode
// coordinator — the topology pkg/router builds — and returns the model the
// specialist ran on. The coordinator delegates to it once, so the
// specialist's declaration surface is the surface of a real delegation
// rather than of a cold start.
func buildSpecialist(t *testing.T, cfg mastagent.TaskAgentConfig) *recordingModel {
	t.Helper()

	taskModel := &recordingModel{name: "rec-task", respond: specialistScript}
	cfg.Name = "task_sub"
	cfg.Description = "Task-mode specialist under test."
	cfg.Model = taskModel

	taskAgent, err := mastagent.NewTaskAgent(cfg)
	if err != nil {
		t.Fatalf("NewTaskAgent: %v", err)
	}
	root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        "coord",
		Description: "Coordinator under test.",
		Model:       &recordingModel{name: "rec-coord", respond: coordinatorScript("task_sub")},
		SubAgents:   []adkagent.Agent{taskAgent},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	runTree(t, root)
	return taskModel
}

// declared folds a recording model's observed declarations into one set and
// fails the test if the model was never called — a declaration test passes
// vacuously against an agent that never reached the model.
func declared(t *testing.T, m *recordingModel) map[string]bool {
	t.Helper()
	if len(m.systems) == 0 {
		t.Fatalf("model %q was never called, so this test proves nothing", m.name)
	}
	out := map[string]bool{}
	for _, d := range m.decls {
		out[d.Name] = true
	}
	return out
}

func names(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	return out
}

// The control case, and the reason the flags exist. A Task specialist under
// a Chat coordinator is offered transfer_to_agent by default, and its only
// possible destination is the coordinator above it: transferTargets skips
// Task-mode agents, so peers are already unreachable.
//
// This test documents current ADK behaviour rather than asserting a mast
// requirement. If ADK stops offering the tool here, delete it — but read the
// treatment case below first, because that is the one that would then be
// enforcing nothing.
func TestTaskAgentIsOfferedTransferByDefault(t *testing.T) {
	m := buildSpecialist(t, mastagent.TaskAgentConfig{})
	if !declared(t, m)[transferTool] {
		t.Skipf("ADK no longer declares %s to a Task sub-agent; the flags are now "+
			"a design choice rather than a crash fix", transferTool)
	}
	// And it arrives undocumented: instructionsForTransferToAgent returns ""
	// for Task and SingleTurn modes, so the model is handed a tool with no
	// instructions telling it when the tool is appropriate.
	if strings.Contains(captured(t, m), transferTool) {
		t.Errorf("ADK now explains %s to Task agents; re-read the flag documentation "+
			"in modes.go, which says it does not", transferTool)
	}
}

// The treatment. Setting both flags empties transferTargets, which makes
// shouldUseAutoFlow false and removes the tool from the request.
func TestDisallowTransferRemovesTheToolFromTheRequest(t *testing.T) {
	m := buildSpecialist(t, mastagent.TaskAgentConfig{
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
	})
	got := declared(t, m)
	if got[transferTool] {
		t.Errorf("the specialist is still offered %s despite both Disallow flags; "+
			"its only target is the coordinator and taking it aborts the run with "+
			"ErrInvalidRunNodeContext. Declared: %v", transferTool, names(got))
	}
	// The flags must not cost the specialist its way of reporting back —
	// removing finish_task would trade a rare fatal transfer for a
	// delegation that can never close.
	if !got["finish_task"] {
		t.Errorf("the specialist has no finish_task, so it has no way to report at all; "+
			"declared: %v", names(got))
	}
}
