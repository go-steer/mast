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

// OnMutation is what happens before a state-mutating tool call
// (docs/orchestration-design.md, hitl_policy.on_mutation). Which calls
// are mutating is the mutation predicate's answer, not this field's:
// built-in annotation or absent MCP readOnlyHint, default-deny-unknown,
// narrowed by the audited tool_catalog.tools[].mutating overrides.
type OnMutation string

const (
	// OnMutationRequireApproval parks the call before it fires and
	// waits for an operator verdict. The default.
	OnMutationRequireApproval OnMutation = "require_approval"

	// OnMutationApply executes mutating calls without asking. For a
	// workload whose blast radius is bounded some other way — a test
	// fixture, a sandbox cluster, an RBAC-confined service account.
	OnMutationApply OnMutation = "apply"

	// OnMutationDryRun never executes a mutating call and tells the
	// agent what would have happened.
	OnMutationDryRun OnMutation = "dry_run"
)

// HITL is the workload's human-in-the-loop policy
// (docs/orchestration-design.md's hitl_policy; the shipped YAML key is
// `hitl:`, and `hitl_policy:` is accepted as the documented spelling —
// see Bundle.HITLPolicy).
type HITL struct {
	// RequireApproval pauses the workflow after each specialist result
	// via a durable RequestInput interrupt; an operator resume supplies
	// the approval verdict.
	//
	// This is the *result*-level gate (the change-safety-gate stand-in
	// from docs/triage-demo-plan.md) and is orthogonal to OnMutation,
	// which gates individual tool calls before they fire.
	RequireApproval bool `yaml:"require_approval,omitempty"`

	// OnMutation is the pre-call write gate's policy. Empty means
	// require_approval: a workload that says nothing about mutation
	// gets the safe answer, because the failure mode of the other
	// default is an unattended agent writing to a production cluster
	// that nobody agreed to.
	//
	// The default binds to the bundle, not to the process. A library
	// embed running with no bundle at all has no workload policy and
	// no channel to answer a park on, so it gets no write gate unless
	// it asks for one; see internal/compose.WriteGate.
	OnMutation OnMutation `yaml:"on_mutation,omitempty"`
}

// EffectiveOnMutation resolves the empty value to the documented
// default.
func (h HITL) EffectiveOnMutation() OnMutation {
	if h.OnMutation == "" {
		return OnMutationRequireApproval
	}
	return h.OnMutation
}

// Dispatch names the root shape a workload wants assembled. The values
// mirror internal/compose.Dispatch; the string lives here so a bundle
// can declare its own shape instead of depending on how it happens to
// be launched. An empty value leaves the choice to the caller.
//
// Precedence is caller-explicit over bundle: `mast serve --dispatch=X`
// wins when the operator actually typed it, the bundle's declaration
// wins otherwise. A shape is a property of the roster (fan-out needs
// read-only analysts and a synthesis specialist; graph needs a
// SingleTurn classifier and a `_fallback`), so the bundle is where it
// belongs — the flag stays for overriding one run.
const (
	// DispatchCoordinator is the SubAgents coordinator shape.
	DispatchCoordinator = "coordinator"
	// DispatchGraph is the workflow-graph LLM-as-router shape.
	DispatchGraph = "graph"
	// DispatchFanout is the concurrent-analysts + synthesis shape.
	DispatchFanout = "fanout"
	// DispatchAuto picks a shape from the roster.
	DispatchAuto = "auto"
)

// Fanout configures the fan-out dispatch shape (docs/v0.3-plan.md W3).
// Ignored under any other dispatch.
type Fanout struct {
	// MaxConcurrency caps how many analyst branches run at once. It is
	// passed to ADK's NewParallelWorker, which is the only concurrency
	// knob that binds from a workflowagent root (resolved-decision row
	// 133 — WithMaxConcurrency is unreachable there). 0 means the
	// package default; negative means unbounded, which is ADK's own
	// meaning for the argument and is why the field is not clamped.
	MaxConcurrency int `yaml:"max_concurrency,omitempty"`
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

// A2A is the workload's A2A-server exposure (docs/a2a-design.md, "Which
// agents get exposed"). Absent or expose:false means the workload is not
// reachable over A2A — exposure has real ops implications (auth setup,
// external contract stability), so it is opt-in per workload.
type A2A struct {
	// Expose gates the whole section; false (the default) means no A2A
	// skill is published for this workload.
	Expose bool `yaml:"expose,omitempty"`

	// SkillName is the A2A skill id published on the agent card and named
	// by inbound message/send calls. Defaults to the workload name.
	SkillName string `yaml:"skill_name,omitempty"`

	// SkillDescription is the human-readable skill summary on the card.
	SkillDescription string `yaml:"skill_description,omitempty"`

	// InputSchema / OutputSchema are a MAST-SIDE convention only: mast
	// validates inbound task inputs against them and may render them into
	// the skill description. Spec AgentSkill has no schema fields, so
	// these do NOT round-trip through the agent card as machine-readable
	// schema (docs/a2a-design.md note).
	InputSchema  map[string]any `yaml:"input_schema,omitempty"`
	OutputSchema map[string]any `yaml:"output_schema,omitempty"`

	// Auth is the per-skill auth policy.
	Auth A2AAuth `yaml:"auth,omitempty"`
}

// A2AAuth is the per-skill auth policy within a workload's a2a: section.
type A2AAuth struct {
	// Required declares the skill needs authentication. Informational in
	// v0.2 when a server-wide token validator is configured (all exposed
	// skills sit behind it); Scopes are the enforced grain.
	Required bool `yaml:"required,omitempty"`

	// Scopes are the token scopes a caller must carry to invoke the
	// skill; missing scope → 403 (docs/a2a-design.md "Auth model").
	Scopes []string `yaml:"scopes,omitempty"`
}

// AGUI is the workload's AG-UI-server exposure (docs/ag-ui-design.md): the
// agent→user surface a browser/app UI (CopilotKit et al.) drives over an
// HTTP POST + SSE run stream. Absent or expose:false means the workload is
// not reachable over AG-UI — like A2A, exposure carries real ops
// implications (auth setup, a public turn-driving endpoint), so it is opt-in
// per workload.
type AGUI struct {
	// Expose gates the whole section; false (the default) means no AG-UI
	// endpoint is served for this workload.
	Expose bool `yaml:"expose,omitempty"`

	// EndpointPath is the HTTP path the workload is served at. Defaults to
	// "/agui/<name>". Must start with "/".
	EndpointPath string `yaml:"endpoint_path,omitempty"`

	// Description is surfaced in the /agui/agents.json discovery descriptor;
	// defaults to the workload description.
	Description string `yaml:"description,omitempty"`

	// InputSchema is a MAST-SIDE convention only: an optional JSON-Schema-
	// shaped hint surfaced in the discovery descriptor so a client can render
	// an input form. AG-UI's RunAgentInput has no schema field, so this does
	// NOT constrain the wire input.
	InputSchema map[string]any `yaml:"input_schema,omitempty"`

	// SessionModel selects how a run maps to a mast session: "per_thread"
	// (the default — one session per AG-UI threadId, so a chat thread is one
	// continuing conversation) or "per_run" (a fresh session per runId, for
	// stateless one-shot runs). The daemon always derives + namespaces the
	// session id; a client never supplies a raw session id.
	SessionModel string `yaml:"session_model,omitempty"`

	// Auth is the per-endpoint auth policy.
	Auth AGUIAuth `yaml:"auth,omitempty"`
}

// AGUIAuth is the per-endpoint auth policy within a workload's agui: section.
type AGUIAuth struct {
	// Required declares the endpoint needs authentication. Informational in
	// v0.2 when a server-wide token validator is configured (all exposed
	// endpoints sit behind it); Scopes are the enforced grain.
	Required bool `yaml:"required,omitempty"`

	// Scopes are the token scopes a caller must carry to drive a run; missing
	// scope → 403 (docs/ag-ui-design.md "Auth model").
	Scopes []string `yaml:"scopes,omitempty"`
}

// SessionModel values for AGUI.SessionModel.
const (
	// AGUISessionPerThread maps one mast session per AG-UI threadId (the
	// default): a chat thread continues one conversation across runs.
	AGUISessionPerThread = "per_thread"
	// AGUISessionPerRun maps a fresh mast session per AG-UI runId: each run
	// is a stateless one-shot.
	AGUISessionPerRun = "per_run"
)

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

	// Dispatch declares the root shape this roster is built for; empty
	// leaves the choice to the caller. See the Dispatch* constants.
	Dispatch string `yaml:"dispatch,omitempty"`

	// Fanout configures the fan-out shape; ignored under any other
	// dispatch.
	Fanout Fanout `yaml:"fanout,omitempty"`

	// Budget bounds this workload's per-invocation runtime.
	Budget Budget `yaml:"budget,omitempty"`

	// HITL is the human-in-the-loop policy for this workload.
	HITL HITL `yaml:"hitl,omitempty"`

	// HITLPolicy is the spelling docs/orchestration-design.md uses for
	// the same block. The two keys are equivalent and Load folds this
	// one into HITL; declaring both is an error rather than a
	// precedence rule, because a bundle with two conflicting HITL
	// blocks has no reading that is obviously right and the wrong
	// reading executes a mutation nobody approved.
	HITLPolicy HITL `yaml:"hitl_policy,omitempty"`

	// Planner configures the supervisor-body planner for this
	// workload; zero value means planner off.
	Planner Planner `yaml:"planner,omitempty"`

	// EdgeTrigger declares how external signals reach this workload.
	EdgeTrigger EdgeTrigger `yaml:"edge_trigger,omitempty"`

	// A2A declares the workload's A2A-server exposure; zero value (or
	// expose:false) means the workload is not reachable over A2A.
	A2A A2A `yaml:"a2a,omitempty"`

	// AGUI declares the workload's AG-UI-server exposure; zero value (or
	// expose:false) means the workload is not reachable over AG-UI.
	AGUI AGUI `yaml:"agui,omitempty"`

	// Filename is preserved for diagnostics; not part of the on-disk
	// schema.
	Filename string `yaml:"-"`
}
