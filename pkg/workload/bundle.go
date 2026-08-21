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

import (
	"fmt"
	"strings"
	"time"
)

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

	// Precondition declares what a call to this tool assumes about the
	// cluster, as a read this workload can make (v0.4 W7). It is what
	// an approved change set is re-checked against before each of its
	// remaining calls fires: mast takes the read at approval time and
	// again at fire time, and asks the operator afresh if the answer
	// moved.
	//
	// Declaring it is the workload's job because mast cannot derive
	// it. mast is Kubernetes-agnostic and an MCP tool's arguments are
	// opaque to it, so it has no way to know which tool reads the
	// object a write call is about, or which argument names it. A tool
	// with no precondition is bounded by hitl.change_set_ttl alone —
	// which is a real bound, but a clock is not a fact about the
	// cluster, and mast says so in the approval question rather than
	// implying a check it is not making.
	Precondition *Precondition `yaml:"precondition,omitempty"`
}

// Precondition is the bundle spelling of a change-set freshness check
// (approval.Precondition, which this is converted into by
// internal/compose — pkg/approval does not import pkg/workload, the
// same separation MutationPredicate keeps).
//
//	tools:
//	  - name: scale_deployment
//	    mutating: true
//	    precondition:
//	      read: get_deployment
//	      args_from: {name: deployment, namespace: namespace}
//	      fields: [spec.replicas, metadata.generation]
type Precondition struct {
	// Read names the read-only tool that re-establishes the fact. It
	// must be classified read-only: a freshness check that changes the
	// cluster is not a check, and mast refuses to start rather than
	// run one.
	Read string `yaml:"read"`

	// Args are literal arguments for the read.
	Args map[string]any `yaml:"args,omitempty"`

	// ArgsFrom maps a read argument name to the argument of the change
	// being checked that supplies it, so one declaration covers every
	// call to the tool.
	ArgsFrom map[string]string `yaml:"args_from,omitempty"`

	// Fields are dot-separated paths into the read's result to compare
	// individually, so a drifted approval can say what moved. Empty
	// compares the whole result, which is the right default for a
	// narrow read and the wrong one for a chatty read — narrow the
	// read rather than filtering it here.
	Fields []string `yaml:"fields,omitempty"`
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

// Watchdog postures a bundle may declare, in ladder order. These are
// string copies of pkg/watchdog's Mode constants, not the constants
// themselves: this package is stdlib-only by design (it sits in the
// slim-embed slice, and a YAML schema has no business pulling the ADK
// runtime in behind it). TestSafetyWatchdogVocabularyMatchesTheWatchdog
// imports pkg/watchdog from the test binary and fails if the two lists
// ever disagree, so the copy cannot rot silently.
const (
	// WatchdogWarn logs a detected runaway pattern and lets the turn
	// run.
	WatchdogWarn = "warn"

	// WatchdogFeedback warns, and additionally tells the model what it
	// is doing on its next turn.
	WatchdogFeedback = "feedback"

	// WatchdogEnforce feeds back, and additionally halts the session on
	// a Critical alert until an operator resets it.
	WatchdogEnforce = "enforce"
)

// Safety is the workload's runaway-backstop policy: the knobs that
// decide what happens when the agent starts doing something no one
// asked for.
//
// It lives in the bundle rather than only on the command line because
// the bundle is mast's deployment unit. A workload that knows it never
// polls can arm the halt for itself, and a workload that does nothing
// but poll can stay at warn, without every invocation and every deploy
// manifest carrying the flag by hand.
type Safety struct {
	// Watchdog is the behavioral watchdog posture for this workload:
	// warn, feedback, or enforce (pkg/watchdog.Mode's ladder — each
	// rung includes the one below it). Empty means unset, which is not
	// the same as "warn": an unset posture falls through to the
	// daemon's default, and the --watchdog flag overrides whatever is
	// declared here.
	//
	// Enforce is the posture to reach for when the workload's tool loop
	// is bounded by construction — a triage run that reads, concludes,
	// and stops. Leave it unset for a workload whose steady state looks
	// like a loop to a detector: a scheduler-driven daemon watching a
	// rollout settle calls the same tool with the same arguments on
	// purpose, and halting it is the outage.
	Watchdog string `yaml:"watchdog,omitempty"`
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

	// ChangeSetTTL bounds how long an operator's approval of a change
	// set authorizes that set's remaining calls (v0.4 W7). A Go
	// duration string: "10m", "45s". Empty means
	// approval.DefaultGrantTTL.
	//
	// Tune it up rather than down. The TTL is a backstop against an
	// approval executing in a world nobody looked at — a daemon that
	// comes back tomorrow — and not the freshness check itself; that is
	// tool_catalog.tools[].precondition. A TTL short enough to fire
	// during normal operation trains operators to re-approve without
	// reading, which costs more safety than it buys.
	ChangeSetTTL string `yaml:"change_set_ttl,omitempty"`
}

// EffectiveOnMutation resolves the empty value to the documented
// default.
func (h HITL) EffectiveOnMutation() OnMutation {
	if h.OnMutation == "" {
		return OnMutationRequireApproval
	}
	return h.OnMutation
}

// EffectiveChangeSetTTL parses hitl.change_set_ttl. A zero duration
// means "unset, use the package default" — the caller resolves it,
// because the default belongs to the write gate rather than to the
// bundle schema.
//
// The parse error is returned rather than swallowed: a typo'd duration
// silently becoming the default is how a workload ends up with a
// freshness window nobody chose. Load refuses the bundle instead.
func (h HITL) EffectiveChangeSetTTL() (time.Duration, error) {
	if strings.TrimSpace(h.ChangeSetTTL) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(h.ChangeSetTTL)
	if err != nil {
		return 0, fmt.Errorf("hitl.change_set_ttl %q is not a duration (want something like \"10m\"): %w", h.ChangeSetTTL, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("hitl.change_set_ttl %q is not positive; omit it for the default, or set a window an operator's approval should survive", h.ChangeSetTTL)
	}
	return d, nil
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
	// DispatchBounded is the one-call shape: a single SingleTurn
	// specialist with a declared output_schema, no router and no tool
	// loop, so the cost of a cycle is a constant an operator can read
	// off the bundle. Never inferred — a roster has to ask for it.
	DispatchBounded = "bounded"
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

// ScheduledTrigger declares that the workload wakes itself on a fixed
// cadence (v0.4 W4.1) — the trigger for periodic work nobody POSTs to:
// a nightly cost sweep, a drift check, a five-minute look at a cluster
// that is not currently on fire.
//
// The cadence is ANCHORED, not reset: fires land on anchor + k×interval,
// where the anchor is the moment mast first saw this schedule and is
// persisted with it. A daemon restart therefore resumes the original
// phase instead of re-phasing to whenever the process happened to come
// back. Ticks that passed while the daemon was down are skipped rather
// than caught up; the reasoning for that lives next to the code that
// acts on it, in cmd/mast/schedtrigger.go.
//
// A workload may declare this alongside `http:` — the two triggers are
// independent, and an operator can still inject into a scheduled
// workload.
type ScheduledTrigger struct {
	// Interval is the cadence as a Go duration string: "15m", "1h",
	// "24h". Required whenever the block is present, because there is
	// no defensible default period for "how often should this cost
	// money".
	Interval string `yaml:"interval,omitempty"`

	// Jitter bounds a random offset added to each individual fire:
	// a fire lands somewhere in [tick, tick+jitter).
	//
	// Non-zero by default (a tenth of the interval, capped at
	// defaultMaxJitter) because the failure mode it prevents is one
	// nobody declares their way into: N replicas of one deployment,
	// started by one rollout, all waking on the same second and
	// arriving at the same API server together. The offset applies to
	// the fire and never to the anchor, so it cannot accumulate into
	// drift — a jittered cadence is still the cadence that was asked
	// for, sampled a little late. Set "0s" for an exactly-on-the-tick
	// schedule.
	Jitter string `yaml:"jitter,omitempty"`

	// Prompt is the text each scheduled run opens with — what the
	// workload is being woken up to do ("Sweep every namespace for
	// pods in CrashLoopBackOff and report what you find").
	//
	// Optional, but a scheduled run that says nothing is a workload
	// re-reading its own system instruction and guessing; the envelope
	// mast supplies on its own carries only the tick.
	Prompt string `yaml:"prompt,omitempty"`
}

// Cadence bounds. The floor is not a performance guard: every fire is a
// model run with a budget attached, so a sub-second interval is a typo
// that spends money, and refusing it at load is cheaper than noticing it
// on the bill.
const (
	minScheduledInterval = time.Second
	defaultMaxJitter     = 30 * time.Second
)

// EffectiveInterval parses edge_trigger.scheduled.interval. A nil
// trigger (no scheduled block) has no cadence and reports zero.
//
// Like EffectiveChangeSetTTL the parse error is returned rather than
// swallowed: a schedule that silently failed to parse is a workload an
// operator believes is running and that has never once woken up.
func (s *ScheduledTrigger) EffectiveInterval() (time.Duration, error) {
	if s == nil {
		return 0, nil
	}
	raw := strings.TrimSpace(s.Interval)
	if raw == "" {
		return 0, fmt.Errorf("edge_trigger.scheduled names no interval (want something like \"15m\"); a schedule without a cadence never fires")
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("edge_trigger.scheduled.interval %q is not a duration (want something like \"15m\"): %w", s.Interval, err)
	}
	if d < minScheduledInterval {
		return 0, fmt.Errorf("edge_trigger.scheduled.interval %q is under %s; every fire is a full model run, so this is a bill, not a cadence", s.Interval, minScheduledInterval)
	}
	return d, nil
}

// EffectiveJitter resolves edge_trigger.scheduled.jitter against the
// interval, which is why the default is computed here and not left to
// the caller: it is a fraction of a value the same block declares.
//
// An omitted jitter means the default; an explicit "0s" means the
// operator asked for an exact cadence and gets one.
func (s *ScheduledTrigger) EffectiveJitter() (time.Duration, error) {
	if s == nil {
		return 0, nil
	}
	interval, err := s.EffectiveInterval()
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(s.Jitter) == "" {
		return min(interval/10, defaultMaxJitter), nil
	}
	d, err := time.ParseDuration(s.Jitter)
	if err != nil {
		return 0, fmt.Errorf("edge_trigger.scheduled.jitter %q is not a duration (want something like \"30s\"): %w", s.Jitter, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("edge_trigger.scheduled.jitter %q is negative; jitter delays a fire, it cannot pull one earlier", s.Jitter)
	}
	if d >= interval {
		return 0, fmt.Errorf("edge_trigger.scheduled.jitter %q is not smaller than the interval %q; a fire that can slip past the next tick has no cadence left to keep", s.Jitter, s.Interval)
	}
	return d, nil
}

// EdgeTrigger declares how external signals reach this workload: an
// inbound POST, a cadence of its own, or both. Other transports (a
// message queue) will join here.
type EdgeTrigger struct {
	HTTP      *HTTPTrigger      `yaml:"http,omitempty"`
	Scheduled *ScheduledTrigger `yaml:"scheduled,omitempty"`
}

// MonitorCollect is one call mast makes on its OWN behalf at the top of
// a monitoring cycle, before the model is woken (v0.5 W4.2).
//
//	monitor:
//	  collect:
//	    - tool: k8s_cluster_health
//	      as: health
//	    - tool: k8s_findings_diff
//	      args: {transitions: "new,escalated,resolved"}
//	      as: transitions
//
// The result is handed to the model as part of the wake-up envelope. The
// model never holds the tool.
type MonitorCollect struct {
	// Tool is the wired tool to run. It resolves against the same
	// toolsets everything else does, so a name nothing wires is a
	// failed cycle naming the tool — not a silent empty result.
	Tool string `yaml:"tool"`

	// Args are the literal arguments for the call. mast has nothing to
	// derive them from: a collection call is not about any particular
	// object, so unlike a precondition read there is no change to map
	// arguments from.
	Args map[string]any `yaml:"args,omitempty"`

	// As is the key this call's result is filed under in the envelope.
	// Defaults to the tool name, which is the right answer until a
	// workload collects from one tool twice.
	As string `yaml:"as,omitempty"`
}

// Key is the envelope key this call's result is filed under.
func (c MonitorCollect) Key() string {
	if s := strings.TrimSpace(c.As); s != "" {
		return s
	}
	return strings.TrimSpace(c.Tool)
}

// Monitor is the workload's unattended-monitoring block (v0.5 W4.2):
// the calls a scheduled cycle makes for itself, outside the model's tool
// surface.
//
// # Why this is a block and not a specialist's allowlist
//
// The obvious spelling — give the model the collection tools and let it
// call them — does not survive contact with the write gate, and the
// reason is not a mast quirk. A run-to-run finding diff *advances
// persisted state* as a side effect of answering "what changed?", so it
// declares itself mutating and is right to; and mast's mutation
// predicate defaults every MCP tool to mutating regardless, because
// ADK's mcptoolset drops MCP's annotations and default-deny-unknown is
// the only safe reading of a tool nobody classified. Under the default
// hitl.on_mutation: require_approval, a cycle that asks the model to
// call the diff parks for a human on EVERY fire. An unattended monitor
// that needs an operator to authorize finding out whether anything
// changed is not unattended.
//
// So the collection leg is mast's. Nothing gates it because no model
// asked for anything, and internal/compose refuses to start if a tool
// named here also appears in any specialist's reach — the exception is
// narrow by construction rather than by convention.
//
// The corollary is the property scoreboard row 9 is about: a leg the
// model is not part of cannot spend a token.
//
// # What this block does NOT carry
//
// The cadence. That is edge_trigger.scheduled, where v0.4 put it, and
// duplicating it here would be two places to configure one cycle — the
// failure mode being that they disagree and the operator reads the one
// that is not running.
//
// And the transition vocabulary. transitions_from names WHERE the
// classification comes from; what the classes are, and which findings
// deserve which one, belongs to the tool that answers — see pkg/monitor
// for why mast holds no opinion about it.
type Monitor struct {
	// Collect is the ordered list of calls one cycle opens with. Order
	// is honoured and the calls are serial: a diff that classifies the
	// scan has to run after the scan, and there is no useful
	// concurrency in two calls when the second depends on the first.
	Collect []MonitorCollect `yaml:"collect,omitempty"`

	// TransitionsFrom names the collect key whose result carries the
	// run-to-run classification — what is new, what got worse, what
	// cleared since the previous cycle (v0.5 W4.4).
	//
	//	monitor:
	//	  collect:
	//	    - tool: k8s_findings_diff
	//	      args: {transitions: "new,escalated,resolved"}
	//	      as: transitions
	//	  transitions_from: transitions
	//
	// Naming it does three things nothing else can do: the envelope
	// files the parsed set under `transitions` instead of a wall of
	// text; the cycle FAILS on a malformed or truncated answer rather
	// than waking the model with a hole where the diff should be; and
	// W4.5's notifier gets a set to be empty or not, which is what "only
	// notify on change" is decided from.
	//
	// It is optional, and a workload that collects raw facts and lets
	// the model read them is a supported shape — but a workload that
	// wants to notify on change needs the classification named, because
	// mast will not derive one. See pkg/monitor.
	TransitionsFrom string `yaml:"transitions_from,omitempty"`

	// Notify is where a cycle speaks, and how long it may stay silent
	// (v0.5 W4.5). Absent means a cycle that changes nothing outside
	// mast: the assessment lands in the session transcript and nowhere
	// else, which is the right default for a workload nobody has told
	// where to post.
	Notify *MonitorNotify `yaml:"notify,omitempty"`

	// Ack is the tool an operator's acknowledgement is forwarded to
	// (v0.5 W4.6). Absent means the daemon's ack route answers 404: a
	// workload that declares nowhere to forward an ack cannot take one.
	Ack *MonitorAck `yaml:"ack,omitempty"`
}

// MonitorAck is the inbound half of a monitoring cycle (v0.5 W4.6): an
// operator says "I have seen this one, stop telling me", and mast
// forwards that to whoever owns the finding's state.
//
//	monitor:
//	  ack:
//	    tool: k8s_findings_ack
//	    args: {window: 4h}
//
// # Which direction this runs in
//
// Every other monitor block reads. This one writes, and to a store mast
// does not own: the suppression lives with the producer of the
// transitions, because that is the only place the next cycle's
// classification can read it from. mast's part is smaller and is the
// part nothing else can do — authenticate the operator, record who
// asked and when, and forward. The producer owns the window and the
// state; mast owns the attribution.
//
// # Why the model cannot reach it
//
// For the same structural reason a collect tool cannot: an ack tool is
// run by mast on its own behalf, and internal/compose refuses to start
// if it is reachable from any roster. That fence matters more here than
// it does for collection. A suppression is a mute button on the
// monitoring, the producer's own triage-status write is typically not
// permission-gated, and mast's authn is therefore the only thing
// standing in front of it. An agent that could silence its own alerts
// is not a monitoring system.
//
// # What is deliberately not here
//
// A verdict, a grant, a freshness window, a change-set signature. An
// ack is not a mutation approval — see docs/orchestration-design.md,
// "Ack routing" — and giving it the write gate's machinery would put a
// suppression into the decision export as an adjudication, which is how
// "who approved this change" quietly starts meaning "who muted an
// alert".
//
// And a per-request window. How long a suppression lasts is the
// producer's policy, set through args for the deployment; there is no
// consumer for a per-request one until an in-chat ack button exists,
// and a knob mast passes through without understanding is a knob it
// cannot validate.
type MonitorAck struct {
	// Tool is the wired tool an ack is forwarded to. It resolves
	// against the same toolsets everything else does.
	Tool string `yaml:"tool"`

	// Args are literal arguments merged UNDER the two mast supplies:
	// subject_key (what the operator acked) and ack_by (who they are).
	// Deployment facts belong here — which cluster, how long a
	// suppression lasts. An attempt to pin subject_key or ack_by is
	// refused at load, because a bundle that names the acker names it
	// for everyone.
	Args map[string]any `yaml:"args,omitempty"`
}

// MonitorNotify is the egress half of a monitoring cycle (v0.5 W4.5):
// where a cycle that has something to say says it, and how long a
// quiet monitor may go without saying anything at all.
//
//	monitor:
//	  notify:
//	    conversation: C0123ESCALATIONS
//	    digest_after: 6h
//
// # Only on change
//
// A cycle speaks when its classification is non-empty. A cycle whose
// classification is empty — "we looked, and nothing changed" — does
// not wake the model and posts nothing, which is the whole point: an
// unattended monitor's ordinary output is silence, and a channel that
// gets a message every fifteen minutes is a channel nobody reads.
//
// A workload that names no monitor.transitions_from has no basis to be
// quiet on and therefore speaks every cycle. That is a supported
// shape and a loud one; the load error it would deserve does not exist
// because "post every cycle" is a legitimate thing to ask a low-cadence
// workload for.
type MonitorNotify struct {
	// Conversation is the platform key switchboard posts into — for
	// Slack a channel ID, or "channel:thread_ts" to post into a thread.
	// Required whenever the block is present: there is no default
	// channel, and a monitor that guessed one would be worse than one
	// that refused to start.
	Conversation string `yaml:"conversation"`

	// DigestAfter is the longest this workload may stay silent. Once
	// that much wall-clock has passed with nothing to report, the next
	// cycle speaks anyway — a heartbeat, so that silence means "nothing
	// changed" rather than "the daemon died three days ago".
	//
	// Wall-clock rather than a count of quiet cycles (the open question
	// #242 left for W4.5): what an operator wants to state is how long
	// they are willing to be in the dark, and a count only answers that
	// after they have multiplied it by an interval they have to go and
	// look up. A count also silently re-scales when the cadence is
	// retuned, which is precisely when a deadman should not move.
	//
	// Empty or "0s" disables the heartbeat: the workload speaks only
	// when something changed.
	DigestAfter string `yaml:"digest_after,omitempty"`
}

// NotifyTarget returns the conversation a cycle posts into, or "" when
// the workload declares no notify block.
func (m Monitor) NotifyTarget() string {
	if m.Notify == nil {
		return ""
	}
	return strings.TrimSpace(m.Notify.Conversation)
}

// EffectiveDigestAfter parses monitor.notify.digest_after. Zero means
// no heartbeat: this workload speaks only when something changed.
//
// The parse error is returned rather than swallowed, for the reason
// EffectiveInterval gives: a deadman that silently failed to parse is
// an operator believing they will hear from a monitor that has no way
// left to speak.
func (m Monitor) EffectiveDigestAfter() (time.Duration, error) {
	if m.Notify == nil {
		return 0, nil
	}
	raw := strings.TrimSpace(m.Notify.DigestAfter)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("monitor.notify.digest_after %q is not a duration (want something like \"6h\"): %w", m.Notify.DigestAfter, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("monitor.notify.digest_after %q is negative; it is how long silence may last, not how far back to look", m.Notify.DigestAfter)
	}
	return d, nil
}

// Enabled reports whether this workload collects on its own behalf.
func (m Monitor) Enabled() bool { return len(m.Collect) > 0 }

// TransitionsKey is the collect key the classification is read from, or
// "" if the workload does not name one.
func (m Monitor) TransitionsKey() string { return strings.TrimSpace(m.TransitionsFrom) }

// CollectTools returns the tool names the collection leg runs, in
// declaration order and de-duplicated.
func (m Monitor) CollectTools() []string {
	seen := make(map[string]bool, len(m.Collect))
	out := make([]string, 0, len(m.Collect))
	for _, c := range m.Collect {
		name := strings.TrimSpace(c.Tool)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// AckTool is the tool an operator acknowledgement is forwarded to, or
// "" for a workload that takes no acks (v0.5 W4.6).
func (m Monitor) AckTool() string {
	if m.Ack == nil {
		return ""
	}
	return strings.TrimSpace(m.Ack.Tool)
}

// AckArgs returns the bundle's literal ack arguments. The caller merges
// mast's two over the top of these, never under them.
func (m Monitor) AckArgs() map[string]any {
	if m.Ack == nil {
		return nil
	}
	return m.Ack.Args
}

// SelfRunTools returns every tool mast runs on this workload's behalf
// outside the model's dispatch path — the collection calls and the ack
// forward. This is the set internal/compose keeps out of every roster.
//
// One list rather than two because the fence is one rule: a tool mast
// runs ungated must be reachable by nobody else. Which direction it
// runs in is why the exception exists, not what bounds it.
func (m Monitor) SelfRunTools() []string {
	out := m.CollectTools()
	ack := m.AckTool()
	if ack == "" {
		return out
	}
	for _, n := range out {
		if n == ack {
			return out
		}
	}
	return append(out, ack)
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

	// Safety is the workload's runaway-backstop policy; the zero value
	// leaves every posture to the host's default.
	Safety Safety `yaml:"safety,omitempty"`

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

	// Monitor declares the calls a scheduled cycle makes on mast's own
	// behalf, before the model is woken; zero value means the model is
	// woken with nothing but the tick.
	Monitor Monitor `yaml:"monitor,omitempty"`

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
