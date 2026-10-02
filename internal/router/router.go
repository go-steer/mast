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

// Package router assembles the top-level agent for a workload: a
// Chat-mode coordinator with the workload's specialists as SubAgents.
//
// This uses ADK v2's canonical dispatch pattern (see
// google.golang.org/adk/v2/examples/multiagent/task_sub_agent):
//
//   - Chat-mode LlmAgent = the coordinator (top-level driver).
//   - Task-mode LlmAgents in SubAgents = per-failure-mode specialists.
//     ADK auto-installs one `task` tool per Task sub-agent; the
//     coordinator's LLM invokes a specialist by calling that tool.
//   - SingleTurn-mode LlmAgents in SubAgents = cheap classifiers.
//     ADK auto-installs one `single_turn` tool per SingleTurn
//     sub-agent; the coordinator's LLM invokes the classifier by
//     calling that tool.
//
// This is "LLM-as-router" delivered via the SubAgents dispatch pattern
// rather than an explicit workflow.Workflow graph. runner.Runner
// requires the root agent to be a Chat-mode LlmAgent (see runner.go
// isLlmAgent + Mode check), so a bare Workflow cannot be a root agent
// — a graph shape must be composed underneath a coordinator or driven
// out-of-runner.
//
// Explicit workflow.Workflow shapes (fan-out-fan-in, adversarial
// verifier, autonomous loop, etc.) remain available for use cases the
// SubAgents pattern doesn't cover; those land as a follow-on when the
// first shape needs them.
package router

import (
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/workload"
)

// Config describes how to build a coordinator for a given workload.
type Config struct {
	// Bundle is the workload definition. Used for the coordinator's
	// name, description, and roster ordering.
	Bundle workload.Bundle

	// Specialists is the roster of specialist agents, indexed by
	// spec name. Every name in Bundle.Specialists must be present.
	Specialists map[string]adkagent.Agent

	// Model is the model the coordinator itself uses. Specialists may
	// use different models (their construction already bound them).
	Model model.LLM

	// Instruction, when non-empty, replaces the default coordinator
	// system prompt.
	Instruction string
}

// Build assembles the coordinator + SubAgents shape and returns the
// root agent that a runner can drive. It does not construct a
// runner — that's the caller's responsibility (letting the caller
// choose the session service, artifact store, etc.).
func Build(cfg Config) (adkagent.Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("router: Config.Model is required")
	}
	if cfg.Bundle.Name == "" {
		return nil, fmt.Errorf("router: Config.Bundle has no Name")
	}
	subAgents := make([]adkagent.Agent, 0, len(cfg.Bundle.Specialists))
	for _, name := range cfg.Bundle.Specialists {
		sp, ok := cfg.Specialists[name]
		if !ok {
			return nil, fmt.Errorf("router: workload %q references specialist %q which is not loaded", cfg.Bundle.Name, name)
		}
		subAgents = append(subAgents, sp)
	}

	// Instruction precedence: explicit Config.Instruction (verbatim) >
	// bundle-specific coordinator default (below) > the generic
	// mode-level agent.DefaultChatInstruction. Because the
	// bundle-specific default is always non-empty, NewCoordinator's
	// Chat-mode fallback never applies on this path — the workload
	// coordinator knows its roster and beats the generic framing.
	instruction := cfg.Instruction
	if instruction == "" {
		instruction = defaultCoordinatorInstruction(cfg.Bundle)
	}

	return mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        cfg.Bundle.Name + "_coordinator",
		Description: cfg.Bundle.Description,
		Instruction: instruction,
		Model:       cfg.Model,
		SubAgents:   subAgents,
	})
}

// defaultCoordinatorInstruction is the fallback system prompt used
// when the caller doesn't supply one. It nudges the coordinator toward
// the specialist-dispatch pattern; workloads that need something
// different pass their own Instruction. This bundle-aware default
// intentionally shadows the generic agent.DefaultChatInstruction —
// a coordinator built for a named workload should be framed around
// that workload, not around generic operator chat.
func defaultCoordinatorInstruction(b workload.Bundle) string {
	return fmt.Sprintf(`You are the coordinator for the %q workload.

You will receive an incident envelope on each turn. Your job is to
choose the right specialist for the incident and delegate to it, then
summarise the specialist's finding as a short structured "INCIDENT
SUMMARY" block for the operator.

Each specialist is available as a tool. Consult the tool descriptions
to pick the right one for the reported failure mode; fall back to the
"_fallback" specialist when no per-failure-mode specialist applies.

Do not attempt remediations yourself — return analysis only. Be
concise; operators are on-call.`, b.Name)
}
