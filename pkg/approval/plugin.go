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

package approval

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/mast/pkg/permissions"
)

// PluginName is the registered name of the write gate's runner plugin.
const PluginName = "mast-write-gate"

// OnMutation is a workload's policy for what happens when a specialist
// calls a mutating tool (docs/orchestration-design.md,
// hitl_policy.on_mutation).
type OnMutation string

const (
	// OnMutationRequireApproval parks the call and waits for an
	// operator. The default, and the only value that is safe when the
	// roster's remediation tools reach a live cluster.
	OnMutationRequireApproval OnMutation = "require_approval"

	// OnMutationApply executes mutating calls without asking. Policy
	// still applies — a configured deny still denies — but no human is
	// consulted. For workloads whose "mutations" are confined to a test
	// fixture, and for reproducing an approved change set unattended.
	OnMutationApply OnMutation = "apply"

	// OnMutationDryRun never executes a mutating call and reports the
	// call it would have made. The agent keeps working with a truthful
	// "this did not happen" instead of a fabricated success.
	OnMutationDryRun OnMutation = "dry_run"
)

// Valid reports whether p is a policy this package implements. The empty
// string is not valid: callers default it explicitly so that a typo in a
// bundle cannot silently mean "require approval" in one code path and
// "apply" in another.
func (p OnMutation) Valid() bool {
	switch p {
	case OnMutationRequireApproval, OnMutationApply, OnMutationDryRun:
		return true
	}
	return false
}

// Config wires the write gate.
type Config struct {
	// Policy is the workload's hitl_policy.on_mutation. Required.
	Policy OnMutation

	// Mutating classifies a tool by name. Required. mast passes
	// effects.Predicate's classification so the outbox and the write
	// gate can never disagree about what counts as a mutation — a tool
	// that is recorded as an effect is a tool that needs approval.
	Mutating func(toolName string) bool

	// Gate adjudicates policy for a parked call and validates the
	// operator's verdict. Required when Policy is
	// OnMutationRequireApproval; unused otherwise.
	Gate *permissions.Gate

	// ChangeSet, when non-nil, enforces the change-set producer
	// contract (W7.0): a specialist's report may only carry a
	// proposed_change naming a tool this workload declares, with
	// arguments that satisfy that tool's declared input schema.
	//
	// It rides the write gate rather than being a plugin of its own
	// for the reason W2.4 paid for: every runner construction path
	// already registers this one, and a check that only some paths
	// install is a check with a hole in it. Nil is "this composition
	// has no catalog to check against" — a library embed with no
	// bundle — and leaves reports untouched.
	ChangeSet *ChangeSetChecker

	// Logger receives the audit trail. Defaults to slog.Default().
	Logger *slog.Logger
}

// New builds the write gate as an ADK runner plugin.
//
// Register it AFTER the pkg/effects outbox plugin at every runner
// construction site. ADK runs before-tool callbacks in registration
// order and the first non-nil response wins, so outbox-first is what
// makes a replayed effect skip the gate: the mutation already happened,
// and asking an operator to approve it again would invite them to
// approve doing it twice (resolved-decision row 144).
func New(cfg Config) (*plugin.Plugin, error) {
	g, err := newWriteGate(cfg)
	if err != nil {
		return nil, err
	}
	return plugin.New(plugin.Config{
		Name:               PluginName,
		BeforeToolCallback: g.beforeTool,
	})
}

// newWriteGate validates the config and builds the gate. Separate from
// New so a test can drive the real callback inside an agent tree it
// builds itself, rather than a hand-written stand-in that could diverge
// from what a daemon registers.
func newWriteGate(cfg Config) (*writeGate, error) {
	if !cfg.Policy.Valid() {
		return nil, fmt.Errorf("approval: Config.Policy %q is not one of %s, %s, %s", cfg.Policy,
			OnMutationRequireApproval, OnMutationApply, OnMutationDryRun)
	}
	if cfg.Mutating == nil {
		return nil, fmt.Errorf("approval: Config.Mutating is required")
	}
	if cfg.Policy == OnMutationRequireApproval && cfg.Gate == nil {
		return nil, fmt.Errorf("approval: Config.Gate is required under %s: without a gate there is nothing to decide policy, and a write gate that cannot refuse is not a gate", OnMutationRequireApproval)
	}
	g := &writeGate{cfg: cfg}
	if g.cfg.Logger == nil {
		g.cfg.Logger = slog.Default()
	}
	return g, nil
}

type writeGate struct {
	cfg Config
}

// beforeTool is the whole gate. A non-nil returned map is the tool's
// RESPONSE and the tool does not run — ADK names the return value
// newArgs, which is misleading; see adkseam_test.go, which pins it.
// Returning nil runs the tool with args exactly as they stand, including
// any in-place edit made here.
func (g *writeGate) beforeTool(ctx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	// The producer contract runs first and on a different tool: a
	// report is not a mutation, and the check has to happen before the
	// report becomes the specialist's result rather than before the
	// change is executed.
	if refusal := g.checkChangeSet(ctx, t, args); refusal != nil {
		return refusal, nil
	}
	if !g.cfg.Mutating(t.Name()) {
		return nil, nil
	}
	key := CallKey(t.Name(), args)

	// Second pass. The operator has answered and ADK has re-dispatched
	// the original call with the confirmation attached.
	if c := ctx.ToolConfirmation(); c != nil {
		return g.honorVerdict(ctx, t, key, args, c)
	}

	switch g.cfg.Policy {
	case OnMutationApply:
		g.audit(ctx, t, key, "apply", "policy is apply; executing without operator approval")
		return nil, nil

	case OnMutationDryRun:
		g.audit(ctx, t, key, "dry_run", "policy is dry_run; reporting the call instead of making it")
		return map[string]any{
			"status": "dry_run",
			"tool":   t.Name(),
			"args":   args,
			"detail": "This workload runs with hitl_policy.on_mutation=dry_run. The call above was NOT made and nothing changed. " +
				"Treat it as a proposal: report what you would have done and continue with read-only work. Do not retry.",
		}, nil
	}

	// require_approval. Policy adjudicates first: a configured deny is
	// not a question worth putting to an operator.
	err := g.cfg.Gate.CheckMutatingToolCall(ctx, t.Name(), key)
	if !errors.Is(err, permissions.ErrApprovalRequired) {
		g.audit(ctx, t, key, "denied_by_policy", err.Error())
		return map[string]any{
			"error": "denied_by_policy",
			"detail": "This call is refused by the operator's configured permission policy, not by a person who might reconsider: " + err.Error() +
				" Do not retry this call and do not attempt the same change by another route. Report the refusal.",
		}, nil
	}

	// Park. RequestConfirmation writes the question into the session
	// event log as a long-running function call, which is what makes the
	// pause outlive this process (scoreboard row 5).
	hint := fmt.Sprintf("Approve mutating call %s?", key)
	if err := ctx.RequestConfirmation(hint, Request{
		Tool:    t.Name(),
		Args:    args,
		Key:     key,
		Policy:  string(g.cfg.Policy),
		Agent:   ctx.AgentName(),
		Verdict: verdictHelp,
	}); err != nil {
		return nil, fmt.Errorf("approval: requesting confirmation for %s: %w", t.Name(), err)
	}
	g.audit(ctx, t, key, "awaiting_approval", "parked for operator approval")
	return map[string]any{
		"status": "awaiting_operator_approval",
		"detail": "This mutating call is parked pending operator approval and has NOT been made. " +
			"It will be made, made with edited arguments, or refused, depending on what the operator decides. " +
			"Do not retry this call, do not attempt the same change by another route, and do not treat this as a failure. " +
			"Finish any read-only work, report that the change awaits approval, and stop.",
	}, nil
}

// honorVerdict runs on the resumed call, with the operator's answer
// attached. Anything it cannot make sense of refuses the call: the
// failure mode of a permissive reading here is mutating a cluster on the
// strength of a payload nobody vouched for.
//
// args is the live map ADK will hand the tool. An edited verdict is
// applied to it in place, and everything downstream of the edit — the
// deny policy, the grant scope, the audit record — is adjudicated
// against the arguments that will actually run, never the ones the model
// proposed.
func (g *writeGate) honorVerdict(ctx agent.Context, t tool.Tool, key string, args map[string]any, c *toolconfirmation.ToolConfirmation) (map[string]any, error) {
	v, err := DecodeVerdict(c)
	if err != nil {
		g.audit(ctx, t, key, "malformed_verdict", err.Error())
		return map[string]any{
			"error":  "malformed_verdict",
			"detail": err.Error() + " The call was not made. Report this to the operator; do not retry.",
		}, nil
	}

	// The call under adjudication. An edit replaces it wholesale: from
	// here on, key and edited are what the gate is deciding about.
	effKey, edited := key, map[string]any(nil)
	if v.Verdict == OutcomeEdit {
		refusal, norm := g.prepareEdit(ctx, t, key, v)
		if refusal != nil {
			return refusal, nil
		}
		edited = norm
		effKey = CallKey(t.Name(), norm)
	}

	d, err := v.Decision()
	if err != nil {
		g.audit(ctx, t, effKey, "malformed_verdict", err.Error())
		return map[string]any{
			"error":  "malformed_verdict",
			"detail": err.Error() + " The call was not made.",
		}, nil
	}

	if err := g.cfg.Gate.RecordMutationVerdict(ctx, t.Name(), effKey, d); err != nil {
		code := "denied_by_operator"
		detail := "The operator refused this call. Do not retry it and do not attempt the same change by another route. " +
			"Report the refusal, including the operator's reason if one was given, and stop."
		if errors.Is(err, permissions.ErrGrantScopeRefused) {
			code = "approval_scope_refused"
			detail = "The approval asked to authorize more than this one call, which mast does not allow for a mutating tool. " +
				"The call was NOT made. Report that the approval must be re-issued for this single call."
		}
		g.audit(ctx, t, effKey, code, err.Error())
		out := map[string]any{"error": code, "detail": detail}
		if v.Note != "" {
			out["operator_note"] = v.Note
		}
		if v.Approver != "" {
			out["approver"] = v.Approver
		}
		return out, nil
	}

	if edited != nil {
		// Last, and only once the verdict has survived every check: the
		// live map ADK is about to hand the tool becomes the operator's.
		record := AppliedEdit{
			Tool:         t.Name(),
			Approver:     v.Approver,
			ProposedKey:  key,
			ExecutedKey:  effKey,
			ProposedArgs: copyArgs(args),
			ExecutedArgs: copyArgs(edited),
			Note:         v.Note,
		}
		applyArgs(args, edited)
		g.recordEdit(ctx, t, record)
	}

	g.cfg.Logger.Info("mutating tool call approved by operator",
		"tool", t.Name(), "call", effKey, "approver", v.Approver, "note", v.Note,
		"edited", edited != nil, "proposed_call", key,
		"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
		"function_call_id", ctx.FunctionCallID())
	return nil, nil
}

// prepareEdit validates an edited verdict and re-adjudicates the deny
// policy against the arguments the operator actually wants run. It
// returns either a refusal to hand the model, or the normalized
// arguments to execute.
//
// The policy re-check is the load-bearing half. CheckMutatingToolCall
// ran before the park, against the model's call; an edit produces a
// different call, and a configured deny that matched the operator's
// version would otherwise be bypassed by the very act of approving —
// "deny scale_deployment(deployment=prod-*)" must not be defeatable by
// editing an approved staging call into a production one.
func (g *writeGate) prepareEdit(ctx agent.Context, t tool.Tool, key string, v Verdict) (refusal, edited map[string]any) {
	// An edit executes arguments no model proposed and no policy pattern
	// vetted, so the audit record is the only trace of where they came
	// from. An unattributed one is not a record (W2.3: the approver is
	// stamped at the resume boundary from the authenticated caller, and
	// every mast path stamps one).
	if v.Approver == "" {
		const detail = "an edited verdict arrived with no approver; mast will not run operator-authored arguments it cannot attribute"
		g.audit(ctx, t, key, "edit_unattributed", detail)
		return map[string]any{
			"error": "edit_unattributed",
			"detail": "The operator's edited arguments arrived without an authenticated approver, so mast refused them. " +
				"The call was NOT made, with either the original or the edited arguments. Report this and stop.",
		}, nil
	}
	norm, err := normalizeEdit(t, v.Args)
	if err != nil {
		g.audit(ctx, t, key, "edit_refused", err.Error())
		return map[string]any{
			"error": "edit_refused",
			"detail": "The operator's edited arguments were refused: " + err.Error() +
				". The call was NOT made, with either the original or the edited arguments. Report this and stop.",
		}, nil
	}
	editedKey := CallKey(t.Name(), norm)
	err = g.cfg.Gate.CheckMutatingToolCall(ctx, t.Name(), editedKey)
	if !errors.Is(err, permissions.ErrApprovalRequired) {
		g.audit(ctx, t, editedKey, "denied_by_policy", err.Error())
		return map[string]any{
			"error": "denied_by_policy",
			"detail": "The operator's edited call is refused by the configured permission policy, not by a person who might reconsider: " + err.Error() +
				" The call was NOT made. Do not retry it and do not attempt the same change by another route.",
		}, nil
	}
	return nil, norm
}

// recordEdit writes the durable "what actually ran" record. It rides the
// state delta of the event ADK is already about to append for this tool
// call, so it lands in the same session, in the same write, with no
// second writer — which matters: the runner owns that row's write lease
// (mast #45/#46).
func (g *writeGate) recordEdit(ctx agent.Context, t tool.Tool, e AppliedEdit) {
	raw, err := json.Marshal(e)
	if err != nil {
		// Refusing the call here would be worse than a thin record: the
		// operator approved it and the policy cleared it.
		g.cfg.Logger.Error("write gate: applied an edit but could not encode its audit record",
			"tool", t.Name(), "call", e.ExecutedKey, "error", err.Error())
		return
	}
	if a := ctx.Actions(); a != nil {
		if a.StateDelta == nil {
			a.StateDelta = map[string]any{}
		}
		a.StateDelta[EditStateKey(ctx.FunctionCallID())] = string(raw)
	}
	g.cfg.Logger.Info("write gate: operator edit applied",
		"outcome", "edit_applied", "tool", t.Name(),
		"call", e.ExecutedKey, "proposed_call", e.ProposedKey,
		"approver", e.Approver, "agent", ctx.AgentName(),
		"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
		"function_call_id", ctx.FunctionCallID())
}

// copyArgs snapshots an argument map for the audit record, so the record
// is not aliased to the live map applyArgs is about to rewrite.
func copyArgs(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (g *writeGate) audit(ctx agent.Context, t tool.Tool, key, outcome, detail string) {
	attrs := []any{"outcome", outcome, "tool", t.Name(), "call", key, "detail", detail}
	// The invocation identifiers are what make the record findable
	// later, and they are also the only part that can be absent: a
	// caller driving the callback outside a turn has no invocation to
	// name. Log the decision without them rather than taking the turn
	// down over a log line — an audit record is a side effect of the
	// gate's job, never a reason it fails.
	if ctx != nil {
		attrs = append(attrs,
			"agent", ctx.AgentName(), "session", ctx.SessionID(),
			"invocation", ctx.InvocationID(), "function_call_id", ctx.FunctionCallID())
	}
	g.cfg.Logger.Info("write gate", attrs...)
}

// Request is the payload of the parked confirmation: everything an
// operator needs to answer without reconstructing the call from the
// transcript, plus a description of the answer mast accepts.
type Request struct {
	Tool    string         `json:"tool"`
	Args    map[string]any `json:"args"`
	Key     string         `json:"key"`
	Policy  string         `json:"policy"`
	Agent   string         `json:"agent"`
	Verdict map[string]any `json:"verdict_format"`
}

// DecodeRequest reads the parked confirmation's payload back into the
// typed Request. The value is whatever the caller got out of the durable
// log — Parked.Request, or a transcript projection's Payload — which is
// the map JSON leaves behind rather than the struct the gate wrote.
func DecodeRequest(v any) (Request, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Request{}, fmt.Errorf("approval: re-marshalling parked request: %w", err)
	}
	var r Request
	if err := json.Unmarshal(raw, &r); err != nil {
		return Request{}, fmt.Errorf("approval: parked payload is not an approval request: %w", err)
	}
	return r, nil
}

// verdictHelp travels with every parked call so an operator answering
// over a raw HTTP resume — no UI, no docs open — can see the shape of
// the answer in the question.
var verdictHelp = map[string]any{
	"verdict": "approve | reject | edit",
	"scope":   "once (default; the only scope a mutating call accepts)",
	"args":    "edit only: the arguments to run instead of the agent's",
	"note":    "optional; shown to the agent",
}

// CallKey renders a tool call as the one-line description an operator
// approves and the deny policy matches against. Keys are sorted so the
// same call always renders the same way — an approval record that
// depended on Go's map iteration order would be useless as an audit
// trail and unmatchable as a policy pattern.
//
// Values are rendered as compact JSON and long ones are elided: the key
// is for a human and a glob, and a 40KB manifest argument in a log line
// helps neither. The full arguments travel in the confirmation payload,
// which is what the operator actually inspects.
func CallKey(name string, args map[string]any) string {
	if len(args) == 0 {
		return name + "()"
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+renderValue(args[k]))
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

const maxValueLen = 120

func renderValue(v any) string {
	if s, ok := v.(string); ok {
		return elide(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return elide(string(b))
}

func elide(s string) string {
	if len(s) <= maxValueLen {
		return s
	}
	return s[:maxValueLen] + "…"
}
