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

// Package agent provides the bucket-1 shim over ADK v2's runner and
// llmagent primitives. It hides the llmagent.Config boilerplate behind
// mast-shaped constructors and encodes the mode conventions the design
// corpus uses (Task-mode specialists, SingleTurn classifiers, Chat-mode
// coordinators).
package agent

import (
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// TaskAgentConfig bundles the parameters for constructing a Task-mode
// LlmAgent. Task-mode agents auto-install the ADK-provided finish_task
// helper; the value the agent passes to finish_task becomes the agent's
// output.
type TaskAgentConfig struct {
	Name         string
	Description  string
	Instruction  string
	Model        model.LLM
	Tools        []tool.Tool
	Toolsets     []tool.Toolset
	OutputSchema *genai.Schema
}

// NewTaskAgent constructs a Task-mode agent. Suitable for
// per-failure-mode specialists (diagnose, remediate, return a structured
// digest).
func NewTaskAgent(cfg TaskAgentConfig) (adkagent.Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent: TaskAgent %q has no Model", cfg.Name)
	}
	return llmagent.New(llmagent.Config{
		Name:         cfg.Name,
		Description:  cfg.Description,
		Instruction:  cfg.Instruction,
		Model:        cfg.Model,
		Tools:        cfg.Tools,
		Toolsets:     cfg.Toolsets,
		OutputSchema: cfg.OutputSchema,
		Mode:         llmagent.ModeTask,
	})
}

// SingleTurnAgentConfig bundles the parameters for constructing a
// SingleTurn-mode LlmAgent. SingleTurn agents run exactly one model
// call; no finish_task loop. Cheap and predictable — the shape behind
// LLM-as-router classifiers.
type SingleTurnAgentConfig struct {
	Name         string
	Description  string
	Instruction  string
	Model        model.LLM
	InputSchema  *genai.Schema
	OutputSchema *genai.Schema
}

// NewSingleTurnAgent constructs a SingleTurn-mode agent.
func NewSingleTurnAgent(cfg SingleTurnAgentConfig) (adkagent.Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent: SingleTurnAgent %q has no Model", cfg.Name)
	}
	return llmagent.New(llmagent.Config{
		Name:         cfg.Name,
		Description:  cfg.Description,
		Instruction:  cfg.Instruction,
		Model:        cfg.Model,
		InputSchema:  cfg.InputSchema,
		OutputSchema: cfg.OutputSchema,
		Mode:         llmagent.ModeSingleTurn,
	})
}
