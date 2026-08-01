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

// Package workload loads workload bundles — the declarative operational
// profile for a mast deployment. A bundle enumerates the MCP servers,
// specialists, budget, and edge-trigger configuration for one named
// workload.
//
// Schema authority: docs/orchestration-design.md defines the canonical
// bundle schema. This package implements the spike subset needed for
// the GKE triage anchor use case (see docs/triage-demo-plan.md) plus
// the v0.1 planner scaffold knob (planner.enabled). Fields beyond that
// — planner review/shape knobs, isolation scope, bundle learning — are
// omitted here and will be added when their downstream subsystems
// land.
package workload

// Mode is the session mode a workload runs in.
type Mode string

const (
	// ModeSingleSession is the spike default: one long-lived session
	// per workload. Multi-session substrate is deferred to v0.2.
	ModeSingleSession Mode = "single_session"

	// ModeMultiSession will be honored once the multi-session substrate
	// lands. Kept in the vocabulary so bundles can declare intent
	// today.
	ModeMultiSession Mode = "multi_session"
)

// MCPServerRef references an MCP server by its declared name in the
// deployment's mcp.json.
type MCPServerRef struct {
	Server string `yaml:"server"`
}

// ToolCatalog is the workload-scoped tool inventory. Composes with (and
// is intersected against) per-specialist tool allowlists at dispatch
// time — see docs/specialists-design.md "Allowlist semantics".
type ToolCatalog struct {
	MCP []MCPServerRef `yaml:"mcp,omitempty"`

	// Tools carries per-tool policy overrides. v0.2 subset: the
	// mutation-class override consumed by the recorded-effect outbox
	// (docs/orchestration-design.md's mutation predicate — MCP
	// annotations are advisory AND dropped by ADK's mcptoolset, so
	// unknown tools default to mutating; this is the audited un-gate
	// for known-safe tools).
	Tools []ToolPolicy `yaml:"tools,omitempty"`
}

// ToolPolicy is a per-tool policy override in the workload's
// tool_catalog, keyed by the tool's registered name.
type ToolPolicy struct {
	Name string `yaml:"name"`

	// Mutating overrides the tool's mutation classification: false
	// un-gates a known-read-only tool from the recorded-effect outbox,
	// true forces the check for a tool the defaults would miss. Nil
	// (omitted) means no override. Applications are audit-logged.
	Mutating *bool `yaml:"mutating,omitempty"`
}

// Budget is the workload-level runtime budget ceiling. Composes over
// per-specialist budgets — the tightest cap wins.
type Budget struct {
	// MaxTurns caps the number of model calls per session. 0 means
	// unlimited. One "turn" = one model call (the unit pkg/budget's
	// meter counts), so a Task specialist's internal tool loop spends
	// one turn per model call, not one per dispatch.
	MaxTurns            int     `yaml:"max_turns,omitempty"`
	MaxWallclockSeconds int     `yaml:"max_wallclock_seconds,omitempty"`
	MaxCostUSD          float64 `yaml:"max_cost_usd,omitempty"`
}

// HITL is the workload's human-in-the-loop policy. Spike subset of
// docs/orchestration-design.md's hitl_policy: a single boolean gating
// specialist results behind operator approval (the change-safety-gate
// stand-in from docs/triage-demo-plan.md).
type HITL struct {
	// RequireApproval pauses the workflow after each specialist result
	// via a durable RequestInput interrupt; an operator resume supplies
	// the approval verdict.
	RequireApproval bool `yaml:"require_approval,omitempty"`
}

// Planner is the workload's planner block (docs/orchestration-design.md
// "The planner"). v0.1 scaffold subset: enabled only. Later fields —
// plan_review_required, reference_shapes — join when their subsystems
// land (v0.2 per the phasing table).
type Planner struct {
	// Enabled switches the workload's root agent to the supervisor-body
	// planner (pkg/planner) with the bundle's specialists as its
	// invoke_specialist roster. When false (the default), dispatch is
	// unchanged: the --dispatch coordinator/graph shapes drive the
	// roster directly.
	Enabled bool `yaml:"enabled,omitempty"`
}

// HTTPTrigger declares that a workload accepts inbound POSTs on the
// mast inject endpoint. The path + auth mode are informational for the
// spike (the inject server declares its own routes globally); later
// steps will wire per-workload path prefixes.
type HTTPTrigger struct {
	Path string `yaml:"path,omitempty"`
	Auth string `yaml:"auth,omitempty"`
}

// EdgeTrigger declares how external signals reach this workload. The
// spike supports HTTP only; other transports (message queue,
// scheduled) will join here.
type EdgeTrigger struct {
	HTTP *HTTPTrigger `yaml:"http,omitempty"`
}

// Bundle is the loaded workload bundle.
type Bundle struct {
	// Name is the workload identifier — unique per mast deployment.
	Name string `yaml:"name"`

	// Description is a human-readable summary used in operator UIs and
	// logs.
	Description string `yaml:"description,omitempty"`

	// Mode declares the session mode. Defaults to single_session.
	Mode Mode `yaml:"mode,omitempty"`

	// ToolCatalog enumerates the tools available to this workload.
	ToolCatalog ToolCatalog `yaml:"tool_catalog,omitempty"`

	// Specialists lists the specialist names this workload composes.
	// Names resolve against the .agents/specialists/ directory (or the
	// spike's --specialists-dir).
	Specialists []string `yaml:"specialists,omitempty"`

	// Budget bounds this workload's per-invocation runtime.
	Budget Budget `yaml:"budget,omitempty"`

	// HITL is the human-in-the-loop policy for this workload.
	HITL HITL `yaml:"hitl,omitempty"`

	// Planner configures the supervisor-body planner for this
	// workload; zero value means planner off.
	Planner Planner `yaml:"planner,omitempty"`

	// EdgeTrigger declares how external signals reach this workload.
	EdgeTrigger EdgeTrigger `yaml:"edge_trigger,omitempty"`

	// Filename is preserved for diagnostics; not part of the on-disk
	// schema.
	Filename string `yaml:"-"`
}
