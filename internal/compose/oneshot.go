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
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/taskclass"
)

// BuildClassRoot constructs the runnable root agent for one public
// task class — the shape behind cmd/mast's one-shot path
// (`mast --task=<class> "<prompt>"`). The class → mode mapping is
// pkg/taskclass's (docs/orchestration-design.md "Public task
// classes"):
//
//   - chat → a Chat-mode coordinator (pkg/agent.NewCoordinator).
//   - debug / implement / research / review → a Task-mode agent,
//     wrapped in a one-node workflow root because ADK v2.1.0's runner
//     only accepts Chat-mode LlmAgent roots directly (same idiom as
//     pkg/planner.NewRoot).
//   - orchestrate → the planner-enabled root (pkg/planner.NewRoot)
//     with an empty specialist roster — the planner scaffold runs and
//     reports honestly that no specialists are declared; a roster
//     needs a workload bundle, which is serve-mode territory in v0.1.
//
// Instruction precedence (pkg/taskclass modes.go): the class profile's
// per-class default is passed explicitly, so it beats the generic
// per-mode fallback; classes without per-class text (chat) fall
// through to pkg/agent's mode default via the constructor's own
// empty-Instruction rule.
// pauseRecorder enables the planner classes' pause_session tool when
// the caller has a durable store (nil otherwise — an in-memory pause
// would die with the process).
func BuildClassRoot(class string, llm model.LLM, pauseRecorder planner.PauseRecorder) (adkagent.Agent, error) {
	if llm == nil {
		return nil, fmt.Errorf("compose: BuildClassRoot requires a model")
	}
	mode := taskclass.AgentMode(class)
	if mode == "" {
		return nil, fmt.Errorf("compose: unknown task class %q (want one of: %s)",
			class, strings.Join(taskclass.Classes(), ", "))
	}

	if taskclass.PlannerEnabled(class) {
		return planner.NewRoot(planner.Config{
			Name:          "mast_" + class,
			Description:   "One-shot planner root for --task=" + class + ".",
			Model:         llm,
			PauseRecorder: pauseRecorder,
		})
	}

	if mode == taskclass.ModeChat {
		return mastagent.NewCoordinator(mastagent.CoordinatorConfig{
			Name:        "mast_" + class,
			Description: "One-shot Chat-mode agent for --task=" + class + ".",
			Instruction: taskclass.Instruction(class),
			Model:       llm,
		})
	}

	a, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        "mast_" + class,
		Description: "One-shot Task-mode agent for --task=" + class + ".",
		Instruction: taskclass.Instruction(class),
		Model:       llm,
	})
	if err != nil {
		return nil, fmt.Errorf("compose: build %s agent: %w", class, err)
	}

	// Task-mode agents cannot be runner roots (ADK v2.1.0: the root
	// must be a Chat-mode LlmAgent or a workflow agent), so wrap the
	// agent as a single workflow node — the planner.NewRoot idiom.
	node, err := workflow.NewAgentNode(a, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("compose: wrap %s agent: %w", class, err)
	}
	body := workflow.NewDynamicNode[any, any]("run_"+class,
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (any, error) {
			return workflow.RunNode[any](ctx, node, nil, workflow.WithRaiseOnWait())
		}, workflow.NodeConfig{})
	return workflowagent.New(workflowagent.Config{
		Name:        "mast_" + class + "_root",
		Description: "One-shot root for --task=" + class + ".",
		Edges:       workflow.Chain(workflow.Start, body),
		SubAgents:   []adkagent.Agent{a},
	})
}
