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
// override). Tool allowlists are enforced at Build time (see
// filterToolsets), as is the model override: a spec's `model:` is
// resolved through BuildOptions.Resolve, and a declared override that
// cannot be resolved fails the build rather than falling back to the
// parent's model. Budget fields are parsed here but enforced
// elsewhere, per field:
//
//   - max_wallclock_seconds — enforced in graph dispatch: pkg/graph
//     maps it to workflow.NodeConfig.Timeout on the specialist's
//     AgentNode (the sanctioned per-node wallclock knob).
//   - max_turns — the per-specialist turn cap is not yet enforced;
//     it needs turn counting inside the specialist's own run. The
//     workload-level max_turns ceiling (pkg/budget Limits.MaxTurns)
//     does bound the session as a whole.
//   - max_cost_usd — not yet enforced per specialist, and Build is
//     the wrong place to try: cost is derived from UsageMetadata on
//     the runner's event stream, which Build never sees. Enforcement
//     lands with per-branch attribution — events carry Branch and
//     NodeInfo, so the session meter (pkg/budget) can learn to bucket
//     spend per specialist and apply min(workload-remaining,
//     specialist-cap). Until then the workload-level max_cost_usd is
//     the only cost ceiling; a specialist's max_cost_usd is recorded
//     but has no effect.
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
// docs/specialists-design.md schema for field semantics, and the
// package doc above for the per-field enforcement status:
// MaxWallclockSeconds is enforced (pkg/graph → NodeConfig.Timeout);
// MaxTurns and MaxCostUSD are parsed and preserved but not yet
// enforced per specialist.
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
