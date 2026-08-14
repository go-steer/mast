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

	// DisallowTransferToParent and DisallowTransferToPeers suppress ADK's
	// transfer_to_agent tool. They pass straight through to llmagent.Config,
	// and setting both empties the agent's transfer target list, which makes
	// shouldUseAutoFlow false and removes the tool *and* its instruction block
	// from every request the agent makes.
	//
	// Set both on a Task specialist that hangs off a Chat-mode coordinator.
	// Peers are already unreachable — transferTargets skips Task-mode agents,
	// so a specialist's only offered destination is the coordinator above it —
	// and taking that one destination is fatal: ADK forwards the transfer
	// in-process, so the coordinator's runChat executes under the specialist's
	// node context, and its first act is to re-dispatch the still-unresolved
	// delegation call through workflow.RunNode. That fails with "workflow:
	// RunNode called outside a dynamic node" and the run produces nothing.
	//
	// The flags are right on their own terms and not only as a crash fix.
	// Delegation to a specialist is one-way: a specialist that transfers
	// abandons the question it was asked, and the coordinator gets no digest
	// to merge.
	//
	// The zero value leaves ADK's default (transfer permitted) untouched.
	DisallowTransferToParent bool
	DisallowTransferToPeers  bool
}

// NewTaskAgent constructs a Task-mode agent. Suitable for
// per-failure-mode specialists (diagnose, remediate, return a structured
// digest).
//
// When cfg.Instruction is empty, DefaultTaskInstruction is used. A
// non-empty Instruction is used verbatim — specialists keep full
// control of their prompt; nothing is prepended.
func NewTaskAgent(cfg TaskAgentConfig) (adkagent.Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent: TaskAgent %q has no Model", cfg.Name)
	}
	return llmagent.New(llmagent.Config{
		Name:                     cfg.Name,
		Description:              cfg.Description,
		Instruction:              effectiveInstruction(cfg.Instruction, DefaultTaskInstruction),
		Model:                    cfg.Model,
		Tools:                    cfg.Tools,
		Toolsets:                 cfg.Toolsets,
		OutputSchema:             cfg.OutputSchema,
		DisallowTransferToParent: cfg.DisallowTransferToParent,
		DisallowTransferToPeers:  cfg.DisallowTransferToPeers,
		Mode:                     llmagent.ModeTask,
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
//
// When cfg.Instruction is empty, DefaultSingleTurnInstruction is used.
// A non-empty Instruction is used verbatim — specialists keep full
// control of their prompt; nothing is prepended.
func NewSingleTurnAgent(cfg SingleTurnAgentConfig) (adkagent.Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent: SingleTurnAgent %q has no Model", cfg.Name)
	}
	return llmagent.New(llmagent.Config{
		Name:         cfg.Name,
		Description:  cfg.Description,
		Instruction:  effectiveInstruction(cfg.Instruction, DefaultSingleTurnInstruction),
		Model:        cfg.Model,
		InputSchema:  cfg.InputSchema,
		OutputSchema: cfg.OutputSchema,
		Mode:         llmagent.ModeSingleTurn,
	})
}
