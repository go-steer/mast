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

// Package effects implements the recorded-effect outbox
// (docs/durable-execution-design.md, "Recorded-effect outbox"): the
// runtime guard that makes mutating-tool re-execution ambiguity visible
// and blocking instead of silent, under mast's declared at-least-once
// contract.
//
// The session event log IS the outbox — a durable FunctionCall event is
// the intent record, its paired FunctionResponse the completion record,
// both keyed by (session, invocation, function-call ID). This package
// adds no storage; it only reads history and refuses or replays calls.
//
// The guard ships as an ADK runner plugin so it sits at the one seam
// every tool execution crosses (Flow.callTool wraps MCP, builtin, and
// federation tools alike). All history reads happen once per turn in
// BeforeRun, off the invocation context's session — the per-call tool
// context structurally has no session access (its Session() is
// unconditionally nil in ADK v2.1.0), so the per-call checks consult
// the turn-start snapshot instead. The permission gate's runtime
// wiring will share this layer, with the outbox check running first (a
// replayed result performs no new effect and needs no fresh approval).
package effects

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// Class is a tool's effect classification under the mutation predicate
// (docs/orchestration-design.md, hitl_policy.on_mutation).
type Class int

const (
	// ClassReadOnly tools never pay the outbox check.
	ClassReadOnly Class = iota

	// ClassMutating tools get intent/completion records in the log and
	// are refused in ambiguous-effect mode. Default for unknown tools:
	// MCP annotations are advisory and ADK v2.1.0's mcptoolset drops
	// them entirely (convertTool copies name/description/schemas only),
	// so default-deny-unknown is both the designed and the only
	// implementable stance; operators un-gate known-safe tools via the
	// workload tool_catalog override.
	ClassMutating

	// ClassSpawning tools start sub-runs whose inner tool calls this
	// process cannot individually guard from the spawn site (the
	// planner dispatch runner is a separate in-memory-session runner;
	// run_shape_* likewise when implemented). They carry no records of
	// their own but are refused in ambiguous-effect mode when they
	// arrive through the tool-execution seam. Note the containment
	// boundary: ADK's coordinator re-dispatch of an already-recorded
	// task delegation bypasses that seam — there, containment holds at
	// the inner-call level instead (the sub-run inherits the invocation
	// ID, so its own mutating tool calls are refused individually).
	ClassSpawning
)

// controlCalls are engine control-flow function calls, never tool
// effects: dangling ones are the normal wire shape of a paused or
// mid-flow session (a pending RequestInput IS an unpaired
// FunctionCall) and must not trip ambiguous-effect mode.
var controlCalls = map[string]bool{
	"adk_request_input":               true,
	"adk_request_credential":          true,
	toolconfirmation.FunctionCallName: true, // "adk_request_confirmation"
	"finish_task":                     true, // Task-mode completion signal
	"transfer_to_agent":               true, // coordinator routing
	"task_completed":                  true, // sequential-agent plumbing
	"exit_loop":                       true, // loop-agent termination
	"pause_session":                   true, // v0.2 plane-A self-pause: a park, not an effect (long-running exclusion also covers it)
}

// builtinClasses classifies mast's own registered tools. Names are
// string literals to keep this package dependency-light; a test
// cross-checks them against the pkg/planner and pkg/federation
// constants so drift fails CI.
var builtinClasses = map[string]Class{
	"invoke_specialist":        ClassSpawning,
	"run_shape_llm_router":     ClassSpawning,
	"run_shape_fan_out_fan_in": ClassSpawning,
	"invoke_remote_agent":      ClassMutating, // remote effects are invisible to this process
	"request_operator_input":   ClassReadOnly, // escalation/pause control surface
}

// Predicate classifies a tool by name. See NewPredicate.
type Predicate func(toolName string) Class

// NewPredicate builds the mutation predicate: control surfaces are
// read-only, mast builtins use their registered class, per-tool
// overrides from the workload bundle apply next, and everything else —
// MCP tools included — defaults to mutating (default-deny-unknown).
func NewPredicate(overrides map[string]bool) Predicate {
	return func(name string) Class {
		if controlCalls[name] {
			return ClassReadOnly
		}
		if mutating, ok := overrides[name]; ok {
			if mutating {
				return ClassMutating
			}
			return ClassReadOnly
		}
		if c, ok := builtinClasses[name]; ok {
			return c
		}
		return ClassMutating
	}
}

// DanglingIntent is a durable mutating (or spawning) FunctionCall from
// a prior attempt with no completion in the log — the call may or may
// not have executed, and the window between an external effect
// committing and its completion event persisting cannot be closed,
// only detected.
type DanglingIntent struct {
	ToolName     string
	CallID       string
	InvocationID string
	Timestamp    time.Time
	// EventIndex is the 0-based position of the carrying event among log
	// events with non-nil Content, in log order. It lets a consumer group
	// dangling calls by the event that raised them — the auto-resume
	// repair path (cmd/mast, #41) answers only the calls of a single
	// (the last) function-call event, ADK's single-call-event validation
	// constraint. The outbox itself ignores it.
	EventIndex int
}

// Config configures the outbox plugin.
type Config struct {
	// Predicate classifies tools; required.
	Predicate Predicate

	// SubAgentNames is the set of agent names composed under the
	// runner's root (see SubAgentNames). ADK's coordinator emits task
	// delegations as FunctionCalls NAMED AFTER THE SUB-AGENT, and
	// deliberately leaves them unresolved across user turns (a
	// specialist asking a clarifying question, a HITL pause inside a
	// node) — engine control flow, not effects. Without this exclusion
	// the scan wedges mast's default composition on its happy path.
	SubAgentNames map[string]bool

	// AckedAt returns the operator's effects-acknowledgement watermark
	// for a session, if one exists (pkg/transcript reads it from the
	// companion ops row). Dangling intents at or before the watermark
	// are considered operator-acknowledged and do not trip
	// ambiguous-effect mode. Optional; nil means no acks.
	AckedAt func(ctx context.Context, sessionID string) (time.Time, bool)

	// Logger for refusals, replays, and mode transitions. Optional.
	Logger *slog.Logger
}

// SubAgentNames walks the agent tree from root and returns every
// composed agent's name — the delegation-call exclusion set for
// Config.SubAgentNames. The root's own name is included (harmless: a
// FunctionCall can never be named after the agent issuing it).
func SubAgentNames(root agent.Agent) map[string]bool {
	out := map[string]bool{}
	var walk func(a agent.Agent)
	walk = func(a agent.Agent) {
		if a == nil || out[a.Name()] {
			return
		}
		out[a.Name()] = true
		for _, sub := range a.SubAgents() {
			walk(sub)
		}
	}
	walk(root)
	return out
}

// New builds the outbox as an ADK runner plugin. Attach it via
// runner.Config.PluginConfig at every runner construction site.
func New(cfg Config) (*plugin.Plugin, error) {
	if cfg.Predicate == nil {
		return nil, fmt.Errorf("effects: Config.Predicate is required")
	}
	o := &outbox{
		cfg:   cfg,
		turns: make(map[string]*turnState),
	}
	if o.cfg.Logger == nil {
		o.cfg.Logger = slog.Default()
	}
	return plugin.New(plugin.Config{
		Name:               "mast-effects-outbox",
		BeforeRunCallback:  o.beforeRun,
		BeforeToolCallback: o.beforeTool,
		AfterRunCallback:   o.afterRun,
	})
}

// turnState is the per-invocation snapshot taken at turn start.
// Keyed by invocation ID: resume turns reuse the paused run's
// invocation ID, the daemon serializes turns per session (#62), and
// ADK generates collision-free invocation IDs. Entries are removed by
// afterRun (deferred by the runner on every path).
type turnState struct {
	// dangling mutating/spawning intents from prior attempts,
	// unacknowledged — non-empty means ambiguous-effect mode.
	dangling []DanglingIntent
	// completions maps function-call ID → recorded FunctionResponse
	// payload for mutating-class calls already completed in the log at
	// turn start. Consulted for exact-key replay; nil payloads are
	// recorded too (hasCompletion distinguishes "recorded nil" from
	// "absent" so a nil recorded result can neither re-execute nor
	// bypass the refusal).
	completions map[string]map[string]any
}

type outbox struct {
	cfg   Config
	mu    sync.Mutex
	turns map[string]*turnState
}

// beforeRun snapshots the session history once per turn: dangling
// mutating intents from prior attempts (at this point the current turn
// has made no calls, so every unpaired non-control, non-delegation
// FunctionCall in the log is a prior attempt's) and recorded
// completions for exact-key replay. Never early-exits the run:
// non-mutating work proceeds even in ambiguous-effect mode.
func (o *outbox) beforeRun(ictx agent.InvocationContext) (*genai.Content, error) {
	sess := ictx.Session()
	if sess == nil {
		return nil, nil
	}
	st := scanHistory(sess.Events(), o.cfg.Predicate, o.cfg.SubAgentNames)
	if len(st.dangling) > 0 && o.cfg.AckedAt != nil {
		if ack, ok := o.cfg.AckedAt(ictx, sess.ID()); ok {
			kept := st.dangling[:0]
			for _, d := range st.dangling {
				if d.Timestamp.After(ack) {
					kept = append(kept, d)
				}
			}
			st.dangling = kept
		}
	}
	if len(st.dangling) == 0 && len(st.completions) == 0 {
		return nil, nil
	}
	o.mu.Lock()
	o.turns[ictx.InvocationID()] = st
	o.mu.Unlock()
	if len(st.dangling) > 0 {
		o.cfg.Logger.Warn("ambiguous prior effect: session has unresolved mutating tool calls from an interrupted turn; refusing mutating calls this turn",
			"session", sess.ID(), "invocation", ictx.InvocationID(),
			"dangling", describe(st.dangling))
	}
	return nil, nil
}

// beforeTool is the per-call check, working entirely from the
// turn-start snapshot (the tool context has no session access):
// replay an already-completed call's recorded result, refuse
// mutating/spawning calls in ambiguous-effect mode, and let everything
// else through. Returning a non-nil map makes the runner skip the tool
// and use the map as its result.
func (o *outbox) beforeTool(ctx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	class := o.cfg.Predicate(t.Name())
	if class == ClassReadOnly {
		return nil, nil
	}

	o.mu.Lock()
	st := o.turns[ctx.InvocationID()]
	o.mu.Unlock()
	if st == nil {
		return nil, nil
	}

	// Call-level exact-key replay: a durable completion for this exact
	// function-call ID means the effect already happened — return the
	// recorded result instead of re-executing. A recorded-but-nil
	// payload substitutes an explicit marker: returning nil here would
	// mean "proceed and execute", the exact double-mutation the record
	// exists to prevent.
	if class == ClassMutating {
		if resp, ok := st.completions[ctx.FunctionCallID()]; ok {
			if resp == nil {
				resp = map[string]any{"mast_replayed_effect": true, "note": "effect already recorded as completed; original response was empty"}
			}
			o.cfg.Logger.Info("replaying recorded effect instead of re-executing",
				"tool", t.Name(), "function_call_id", ctx.FunctionCallID(),
				"invocation", ctx.InvocationID())
			return resp, nil
		}
	}

	if len(st.dangling) == 0 {
		return nil, nil
	}
	o.cfg.Logger.Warn("refusing tool call in ambiguous-effect mode",
		"tool", t.Name(), "function_call_id", ctx.FunctionCallID(),
		"invocation", ctx.InvocationID())
	return map[string]any{
		"error": "ambiguous_prior_effect",
		"detail": "this session has mutating tool calls from an interrupted prior turn whose outcome is unknown; " +
			"mutating and sub-run-spawning calls are refused until an operator acknowledges " +
			"(mast sessions ack-effects, or resume --ack-effects, or abort the session). " +
			"Do not retry this call; finish any read-only work and report the situation.",
		"dangling_calls": describe(st.dangling),
	}, nil
}

// afterRun drops the invocation's snapshot; the runner defers this on
// every Run path.
func (o *outbox) afterRun(ictx agent.InvocationContext) {
	o.mu.Lock()
	delete(o.turns, ictx.InvocationID())
	o.mu.Unlock()
}

// scannedCall is one unpaired FunctionCall from pairScan, tagged with
// its effect class and whether it is excluded from the dangling set
// (long-running park, engine control call, or task delegation named
// after a composed sub-agent).
type scannedCall struct {
	intent   DanglingIntent
	class    Class
	excluded bool
}

// pairedScan is the shared result of walking the event log once, pairing
// FunctionCall parts with their FunctionResponse parts. Both the outbox
// turn snapshot (scanHistory) and the auto-resume eligibility/repair
// scan (ScanDangling) derive their outputs from it, so the pairing and
// exclusion rules live in exactly one place (#41 M3).
type pairedScan struct {
	// unpaired lists every non-empty-ID FunctionCall with no later
	// matching FunctionResponse, in first-seen order.
	unpaired []scannedCall
	// completions maps a MUTATING call's ID to its recorded response
	// payload (replay candidates for the outbox); read-only completions
	// are not tracked.
	completions map[string]map[string]any
	// lastCallEventIndex is the EventIndex of the last event carrying any
	// FunctionCall with a non-empty ID (paired or unpaired), or -1.
	lastCallEventIndex int
}

// pairScan walks the event sequence once and pairs FunctionCall parts
// with FunctionResponse parts. Excluded-but-unpaired calls are retained
// in the result (tagged excluded) so a consumer can distinguish them
// from ordinary dangling calls; calls with an empty ID are skipped
// entirely (ADK's PopulateClientFunctionCallID guarantees IDs on
// runner-written events; blank IDs only occur in hand-built logs and
// cannot be keyed, paired, or acknowledged).
func pairScan(events session.Events, pred Predicate, subAgents map[string]bool) *pairedScan {
	ps := &pairedScan{completions: map[string]map[string]any{}, lastCallEventIndex: -1}
	open := map[string]scannedCall{}
	callName := map[string]string{} // call ID → tool name, for completion classification
	var order []string
	evIdx := -1
	for ev := range events.All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		evIdx++
		longRunning := map[string]bool{}
		for _, id := range ev.LongRunningToolIDs {
			longRunning[id] = true
		}
		for _, part := range ev.Content.Parts {
			if part == nil {
				continue
			}
			if fc := part.FunctionCall; fc != nil {
				if fc.Name == toolconfirmation.FunctionCallName {
					// Belt-and-braces for the placeholder guard below:
					// the confirmation request names the original call
					// it gates; that call's pre-pause placeholder
					// response is NOT a completion.
					if id := confirmationGatedCallID(fc.Args); id != "" {
						delete(ps.completions, id)
					}
				}
				if fc.ID == "" {
					continue
				}
				ps.lastCallEventIndex = evIdx
				callName[fc.ID] = fc.Name
				if _, seen := open[fc.ID]; !seen {
					order = append(order, fc.ID)
				}
				open[fc.ID] = scannedCall{
					intent: DanglingIntent{
						ToolName:     fc.Name,
						CallID:       fc.ID,
						InvocationID: ev.InvocationID,
						Timestamp:    ev.Timestamp,
						EventIndex:   evIdx,
					},
					class:    pred(fc.Name),
					excluded: longRunning[fc.ID] || controlCalls[fc.Name] || subAgents[fc.Name],
				}
			}
			if fr := part.FunctionResponse; fr != nil {
				if fr.ID == "" {
					continue
				}
				delete(open, fr.ID)
				// A confirmation-gated call persists a PLACEHOLDER
				// response before the flow pauses for the operator
				// ("awaiting confirmation" — ADK yields it durably by
				// design). It closes the pair (the call is not
				// dangling: it's a control-flow pause) but it is NOT a
				// completion — replaying it would silently swallow the
				// operator-approved re-execution, which re-fires under
				// the ORIGINAL function-call ID.
				if _, pendingConfirmation := ev.Actions.RequestedToolConfirmations[fr.ID]; pendingConfirmation {
					continue
				}
				name := fr.Name
				if name == "" {
					name = callName[fr.ID]
				}
				if pred(name) == ClassMutating {
					ps.completions[fr.ID] = fr.Response
				}
			}
		}
	}
	for _, id := range order {
		if c, ok := open[id]; ok {
			ps.unpaired = append(ps.unpaired, c)
		}
	}
	return ps
}

// scanHistory produces the outbox turn-start snapshot: unpaired
// mutating/spawning calls (dangling intents) and recorded completions
// for mutating calls (replay candidates). Excluded calls (long-running
// parks, control calls, sub-agent delegations) never dangle.
func scanHistory(events session.Events, pred Predicate, subAgents map[string]bool) *turnState {
	ps := pairScan(events, pred, subAgents)
	st := &turnState{completions: ps.completions}
	for _, c := range ps.unpaired {
		if c.excluded {
			continue
		}
		if c.class == ClassMutating || c.class == ClassSpawning {
			st.dangling = append(st.dangling, c.intent)
		}
	}
	sort.Slice(st.dangling, func(i, j int) bool { return st.dangling[i].Timestamp.Before(st.dangling[j].Timestamp) })
	return st
}

// DanglingScan splits a session's unpaired FunctionCalls into the three
// buckets the boot-time auto-resume pass acts on (cmd/mast, #41). It is
// the once-and-only-once eligibility gate's single source of truth.
type DanglingScan struct {
	// Mutating are unpaired mutating- or spawning-class calls: their
	// effect may or may not have committed, so a session with ANY of them
	// is ineligible for auto-resume and is left for an operator ack
	// (regardless of the effects ack watermark — an ack suppresses the
	// outbox refusal but does not pair the call, and synthesizing a
	// response would falsely assert the effect did not happen).
	Mutating []DanglingIntent
	// Repairable are unpaired ordinary read-only calls (a read-only tool
	// cut off mid-execution). They carry no external effect, so the
	// daemon may answer them with a synthetic error FunctionResponse to
	// make the history provider-valid before re-running.
	Repairable []DanglingIntent
	// Deferred are unpaired excluded calls (sub-agent task delegations,
	// and defensively any control/long-running call): engine-reconstruct
	// or operator territory. The daemon must not synthesize responses for
	// these; a candidate carrying any is skipped in slice-1.
	Deferred []DanglingIntent
	// LastCallEventIndex is the EventIndex of the last event carrying any
	// FunctionCall (paired or not), or -1 if none. Repair is only clean
	// when every Repairable call sits in this event (ADK validates a
	// repair message against the latest function-call event).
	LastCallEventIndex int
}

// ScanDangling classifies a session's unpaired FunctionCalls for the
// auto-resume eligibility and repair decisions. It shares pairScan with
// the outbox, so the two can never drift on what "dangling" means.
func ScanDangling(events session.Events, pred Predicate, subAgents map[string]bool) DanglingScan {
	ps := pairScan(events, pred, subAgents)
	out := DanglingScan{LastCallEventIndex: ps.lastCallEventIndex}
	for _, c := range ps.unpaired {
		switch {
		case c.excluded:
			out.Deferred = append(out.Deferred, c.intent)
		case c.class == ClassMutating || c.class == ClassSpawning:
			out.Mutating = append(out.Mutating, c.intent)
		default:
			out.Repairable = append(out.Repairable, c.intent)
		}
	}
	byTS := func(ds []DanglingIntent) {
		sort.Slice(ds, func(i, j int) bool { return ds[i].Timestamp.Before(ds[j].Timestamp) })
	}
	byTS(out.Mutating)
	byTS(out.Repairable)
	byTS(out.Deferred)
	return out
}

// confirmationGatedCallID extracts the original function-call ID an
// adk_request_confirmation call gates, from its args. In-memory the
// value is a *genai.FunctionCall; after the DB JSON round-trip it is a
// map with an "id" key. Empty string when neither shape matches.
func confirmationGatedCallID(args map[string]any) string {
	orig, ok := args["originalFunctionCall"]
	if !ok {
		return ""
	}
	switch v := orig.(type) {
	case *genai.FunctionCall:
		if v != nil {
			return v.ID
		}
	case map[string]any:
		if id, ok := v["id"].(string); ok {
			return id
		}
	}
	return ""
}

// describe renders dangling intents for logs and refusal payloads.
func describe(ds []DanglingIntent) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, fmt.Sprintf("%s (call %s, invocation %s, %s)",
			d.ToolName, d.CallID, d.InvocationID, d.Timestamp.UTC().Format(time.RFC3339)))
	}
	return out
}

// Overrides flattens workload tool_catalog per-tool policies into the
// override map NewPredicate consumes, logging each applied override
// (the audit-logged requirement from the mutation predicate's
// definition). Later entries win on duplicate names; the workload
// loader rejects duplicates so that only matters for hand-built maps.
func Overrides(logger *slog.Logger, policies []ToolPolicy) map[string]bool {
	if len(policies) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	out := make(map[string]bool, len(policies))
	for _, p := range policies {
		name := strings.TrimSpace(p.Name)
		if name == "" || p.Mutating == nil {
			continue
		}
		out[name] = *p.Mutating
		logger.Info("tool mutation-class override applied (workload tool_catalog)",
			"tool", name, "mutating", *p.Mutating)
	}
	return out
}

// ToolPolicy mirrors workload.ToolPolicy without importing pkg/workload
// (which would drag the YAML loader into every library embed that only
// wants the guard). The daemon and library root convert.
type ToolPolicy struct {
	Name     string
	Mutating *bool
}
