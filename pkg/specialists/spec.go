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

// Package specialists loads specialist .tmpl files from disk and turns
// them into ADK v2 agents. .tmpl files are YAML frontmatter (bounded
// by `---`) followed by a Markdown body used as the specialist's system
// prompt.
//
// Schema follows docs/specialists-design.md. This package implements
// the spike subset (name, description, mode, instruction, model
// override); budget and tool allowlist fields are parsed and preserved
// on the Spec but not yet enforced at Build time. Enforcement lands in
// later spike steps.
package specialists

// Mode is the ADK v2 agent mode a specialist runs in.
type Mode string

const (
	// ModeTask is a Task-mode specialist. Runs to a finish_task
	// completion. Default when mode: is absent.
	ModeTask Mode = "Task"

	// ModeSingleTurn is a SingleTurn-mode specialist. Runs exactly one
	// model call. The shape behind LLM-as-router classifiers.
	ModeSingleTurn Mode = "SingleTurn"
)

// Budget captures the per-specialist runtime bounds. See
// docs/specialists-design.md schema for field semantics.
//
// The spike parses and preserves these but does not yet enforce them —
// budget enforcement composes over ADK v2 NodeConfig.Timeout /
// RetryConfig plus mast-side cost interceptors, which are step-11-plus
// scope.
type Budget struct {
	MaxTurns            int     `yaml:"max_turns,omitempty"`
	MaxWallclockSeconds int     `yaml:"max_wallclock_seconds,omitempty"`
	MaxCostUSD          float64 `yaml:"max_cost_usd,omitempty"`
}

// MCPAllowlist is the per-MCP-server tool allowlist for a specialist.
type MCPAllowlist struct {
	Server string   `yaml:"server"`
	Tools  []string `yaml:"tools,omitempty"`
}

// ToolAllowlist is the composite allowlist of built-in tools, MCP
// tools, and skills a specialist may invoke. Enforcement is a later
// spike step; for now these are parsed and preserved.
type ToolAllowlist struct {
	Builtin []string       `yaml:"builtin,omitempty"`
	MCP     []MCPAllowlist `yaml:"mcp,omitempty"`
	Skills  []string       `yaml:"skills,omitempty"`
}

// Frontmatter is the YAML block at the top of a .tmpl file.
type Frontmatter struct {
	Name        string        `yaml:"name,omitempty"`
	Description string        `yaml:"description"`
	Mode        Mode          `yaml:"mode,omitempty"`
	Model       string        `yaml:"model,omitempty"`
	Budget      Budget        `yaml:"budget,omitempty"`
	Tools       ToolAllowlist `yaml:"tools,omitempty"`
}

// Spec is a fully-loaded specialist: parsed frontmatter plus the raw
// Markdown body used as the system prompt.
type Spec struct {
	// Filename is the path (or filename) the spec was loaded from,
	// preserved for diagnostics.
	Filename string

	// Frontmatter fields, promoted for convenience.
	Name        string
	Description string
	Mode        Mode
	Model       string
	Budget      Budget
	Tools       ToolAllowlist

	// Instruction is the body of the .tmpl file — the specialist's
	// system prompt, verbatim.
	Instruction string
}
