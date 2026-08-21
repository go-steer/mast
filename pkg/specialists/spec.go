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
// override) plus the output-schema contract. Tool allowlists are
// enforced at Build time (see filterToolsets), as is the model
// override: a spec's `model:` is resolved through BuildOptions.Resolve,
// and a declared override that cannot be resolved fails the build
// rather than falling back to the parent's model.
//
// A spec may instead declare `tier: small | mid | frontier`, which is
// the portable half of the same override: it names how much model the
// step is worth, not which vendor's model it must run on, so a bundle
// that puts its twelve diagnosers on the cheap tier still runs on
// whichever provider the operator points mast at. It resolves through
// BuildOptions.ResolveTier (internal/compose maps tier → model ID for
// the running provider via pkg/taskclass.ModelForTier) and fails the
// build the same way `model:` does when it cannot be resolved.
// Declaring both on one spec is a load error: they are two answers to
// one question, and picking a winner silently would mean the loser's
// declaration was decoration.
//
// A spec's `output_schema:` names a JSON-Schema document relative to
// the .tmpl file; it is read, normalized and checked at load time (see
// schema.go) and reaches the agent as llmagent.Config.OutputSchema.
// From there ADK enforces it — a violation is an error on both paths,
// never a warning. In Task mode the schema becomes the finish_task
// declaration and an invalid call is rejected back to the model with
// the validation error, so a bad shape cannot become the task's output.
// In SingleTurn mode the reply is validated on the way out and a
// failure propagates as a run error.
//
// Budget fields are parsed here but enforced elsewhere, per field:
//
//   - max_wallclock_seconds — enforced in graph dispatch: pkg/graph
//     maps it to workflow.NodeConfig.Timeout on the specialist's
//     AgentNode (the sanctioned per-node wallclock knob).
//   - max_turns and max_cost_usd — enforced by the session meter, not
//     here: cost and turns are derived from UsageMetadata on the
//     runner's event stream, which Build never sees. The roster's
//     declarations become budget scopes (internal/compose.MeterScopes
//     → budget.Config.Scopes) that the meter buckets by event author,
//     so a specialist with a tighter ceiling than its workload stops
//     the run on its own — see pkg/budget, "Scopes", for the
//     composition rule and its two known limitations.
//
// Attribution is by session.Event.Author, which carries the agent's
// name on every dispatch shape mast builds. Event.Branch is not the
// seam: in the coordinator/sub-agent-tool shape it is empty.
package specialists

import "google.golang.org/genai"

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

// Capability is what a specialist is allowed to do to the world. It is
// the read/write half of the roster split: analysts diagnose, and a
// separate, declared specialist carries out changes.
//
// The field exists because an allowlist alone cannot distinguish "this
// specialist may write" from "somebody added a write tool to a
// diagnoser and nobody noticed". A prompt saying *do not mutate* is not
// a control; a declaration a loader can refuse is. See
// docs/specialists-design.md, "Capability".
type Capability string

const (
	// CapabilityReadOnly is a specialist that may not reach a mutating
	// tool. Default when capability: is absent — the safe direction, and
	// the one most specialists want.
	CapabilityReadOnly Capability = "read_only"

	// CapabilityChangeExecutor is a specialist that may. Declaring it is
	// not an approval: every mutating call it makes still goes to the
	// write gate (pkg/approval) like any other.
	CapabilityChangeExecutor Capability = "change_executor"
)

// Budget captures the per-specialist runtime bounds. See
// docs/specialists-design.md schema for field semantics, and the
// package doc above for where each is enforced: MaxWallclockSeconds by
// graph dispatch (pkg/graph → NodeConfig.Timeout), MaxTurns and
// MaxCostUSD by the session meter (pkg/budget scopes).
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
// tools, and skills a specialist may invoke.
//
// Presence is significant per axis, per the normative table in
// docs/specialists-design.md: an absent field inherits everything on
// that axis, a present-but-empty field denies everything on it, and a
// non-empty field is a whitelist. `mcp: []` is therefore not the same
// declaration as no `mcp:` key at all — see InheritsAllMCP.
//
// That reading is exact for MCP and describes only one of the three
// axes. Builtin is a declaration rather than a grant and Skills is
// refused outright; each field says why below.
type ToolAllowlist struct {
	// Builtin names built-in tools the specialist is declared to use.
	// It is **not** a grant, whatever the normative table's shape
	// suggests: nothing populates BuildOptions.Tools, so every
	// specialist is built holding no built-in tools at all and there is
	// nothing here for a whitelist to narrow. Absent and empty are the
	// same declaration on this axis, unlike MCP's (#219).
	//
	// What reads it are the checks that treat a declaration as a claim
	// to be held to: internal/compose.CheckCapabilitySplit and
	// pkg/graph.checkBranchTools refuse a read_only specialist or a
	// fan-out branch that names a mutating tool here, and the
	// capability startup log reports it as declared write surface.
	// Those all run in the refusing direction, which is safe under
	// either reading — a claim mast holds you to costs nothing when the
	// claim turns out to grant nothing.
	//
	// The one consumer that ran the other way was the write gate's
	// executable surface, which widened what a proposed change could
	// name; that was corrected with #219, because a built-in name there
	// is a promise the executor cannot keep.
	Builtin []string       `yaml:"builtin,omitempty"`
	MCP     []MCPAllowlist `yaml:"mcp,omitempty"`

	// Skills is the third axis of the normative table, and it is the
	// one nothing enforces: Builtin is read by the write gate and the
	// capability-split check, MCP by filterToolsets, and Skills by no
	// production code at all. mast ships no skills runtime — the
	// subsystem docs/skills-design.md schedules for v0.1 has not
	// landed through v0.4 (#211).
	//
	// So the field is parsed and then refused: LoadFile rejects a
	// non-empty list rather than accept a grant it cannot narrow.
	// It stays declared because the wire and file formats are shared
	// with the design corpus and with embedders, and because a
	// silently-dropped unknown key is a worse failure than a named
	// one — but read "present here" as "the format has a place for
	// it", not as "mast honors it".
	Skills []string `yaml:"skills,omitempty"`
}

// InheritsAllMCP reports whether this allowlist leaves the MCP axis
// unrestricted — i.e. the spec declared no `mcp:` key, so the
// specialist is offered every MCP toolset the workload has.
//
// It exists because the distinction it draws is a nil check that reads
// like a typo. `mcp: []` decodes to an empty non-nil slice and means
// *deny every MCP tool*; a missing `mcp:` decodes to nil and means
// *grant them all*. Those are opposite outcomes one character apart, so
// the question gets asked through a named method rather than re-derived
// at each call site — filterToolsets enforces it, and
// internal/compose.CheckCapabilitySplit refuses the inherit-all case for
// a read_only specialist when the workload has a tool catalog.
func (t ToolAllowlist) InheritsAllMCP() bool { return t.MCP == nil }

// Frontmatter is the YAML block at the top of a .tmpl file.
type Frontmatter struct {
	Name        string        `yaml:"name,omitempty"`
	Description string        `yaml:"description"`
	Mode        Mode          `yaml:"mode,omitempty"`
	Model       string        `yaml:"model,omitempty"`
	Tier        string        `yaml:"tier,omitempty"`
	Capability  Capability    `yaml:"capability,omitempty"`
	Budget      Budget        `yaml:"budget,omitempty"`
	Tools       ToolAllowlist `yaml:"tools,omitempty"`

	// OutputSchema is a path to a JSON-Schema document, relative to the
	// .tmpl file's own directory. It is a reference rather than an
	// inline block on purpose — see the comment at the top of schema.go.
	OutputSchema string `yaml:"output_schema,omitempty"`
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
	Tier        string
	Capability  Capability
	Budget      Budget
	Tools       ToolAllowlist

	// Instruction is the body of the .tmpl file — the specialist's
	// system prompt, verbatim.
	Instruction string

	// OutputSchema is the loaded, normalized and checked contract this
	// specialist's output must satisfy, or nil when the spec declares
	// none. Loaded eagerly by LoadFile so a broken schema is a load
	// error rather than a surprise on the first live turn.
	OutputSchema *genai.Schema

	// OutputSchemaPath is the resolved path OutputSchema came from,
	// preserved for diagnostics. Empty when the spec declares none.
	OutputSchemaPath string
}
