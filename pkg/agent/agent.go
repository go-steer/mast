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

package agent

import (
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
)

// CoordinatorConfig bundles the parameters for constructing a Chat-mode
// coordinator with Task or SingleTurn sub-agents. ADK auto-installs a
// task tool for each Task sub-agent and a single_turn tool for each
// SingleTurn sub-agent — the coordinator's LLM invokes sub-agents by
// calling those tools. No explicit agenttool.New call is required for
// this dispatch pattern.
type CoordinatorConfig struct {
	Name        string
	Description string
	Instruction string
	Model       model.LLM
	SubAgents   []adkagent.Agent
	Tools       []tool.Tool
	Toolsets    []tool.Toolset

	// BeforeModelCallbacks and AfterModelCallbacks pass straight through to
	// llmagent.Config, with ADK's semantics: a Before callback that returns a
	// non-nil response short-circuits the model call — and with it every After
	// callback — while an After callback that returns a non-nil response
	// replaces the one the model produced.
	//
	// Here for the same reason as TaskAgentConfig's — see the longer note
	// there — and on both so that the two configs do not differ arbitrarily
	// in what a caller can reach.
	BeforeModelCallbacks []llmagent.BeforeModelCallback
	AfterModelCallbacks  []llmagent.AfterModelCallback
}

// NewCoordinator constructs a Chat-mode LlmAgent with the given
// sub-agents and tools. The coordinator drives the top-level
// conversation; sub-agents handle delegated tasks.
//
// When cfg.Instruction is empty, DefaultChatInstruction is used. A
// non-empty Instruction is used verbatim — callers with a
// bundle-specific prompt (e.g. router.Build's per-workload coordinator
// default) keep full control.
func NewCoordinator(cfg CoordinatorConfig) (adkagent.Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent: Coordinator %q has no Model", cfg.Name)
	}
	return llmagent.New(llmagent.Config{
		Name:        cfg.Name,
		Description: cfg.Description,
		Instruction: effectiveInstruction(cfg.Instruction, DefaultChatInstruction),
		Model:       cfg.Model,
		SubAgents:   cfg.SubAgents,
		Tools:       cfg.Tools,
		Toolsets:    cfg.Toolsets,

		BeforeModelCallbacks: cfg.BeforeModelCallbacks,
		AfterModelCallbacks:  cfg.AfterModelCallbacks,

		Mode: llmagent.ModeChat,
	})
}
