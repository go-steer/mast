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
	"time"

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

	// Grants, when non-nil, lets one operator answer authorize a whole
	// change set (W7): approving a call that belongs to a recorded set
	// with `scope: change_set` mints a grant for each of the set's
	// other calls, bound to that call's exact signature, and this gate
	// consumes them instead of parking again.
	//
	// The value configures how long such an approval speaks for and
	// how mast re-checks the world before each granted call fires. Nil
	// is W7.0 behaviour: every mutating call is parked on its own, and
	// `scope: change_set` is refused rather than quietly treated as
	// `once` — see grant.go.
	Grants *Freshness

	// Captures, when non-nil, records what a mutating call is about to
	// overwrite before it fires, and the call that puts it back (#296).
	//
	// Nil, and a non-nil value under which no tool declares a capture,
	// are the same thing: nothing is read and nothing is recorded. Only
	// a tool whose declaration names a read pays anything — and pays it
	// fail-closed, so a declared capture that cannot be taken stops the
	// call. See capture.go.
	Captures *CaptureRules

	// Workload names the workload whose bundle composed this gate, and
	// is stamped onto every Decision record so an exported adjudication
	// is legible without the session it came from (v0.4 W8). Optional:
	// a library embed that registers this plugin itself has no bundle to
	// name, and an unnamed workload is a thinner row rather than a
	// missing one.
	Workload string

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
		if refusal := g.permit(ctx, t, key, args); refusal != nil {
			return refusal, nil
		}
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
	// not a question worth putting to an operator — and it is not
	// something an earlier approval overrides either, which is why this
	// runs in front of the grant check and not behind it.
	err := g.cfg.Gate.CheckMutatingToolCall(ctx, t.Name(), key)
	if !errors.Is(err, permissions.ErrApprovalRequired) {
		g.audit(ctx, t, key, "denied_by_policy", err.Error())
		return map[string]any{
			"error": "denied_by_policy",
			"detail": "This call is refused by the operator's configured permission policy, not by a person who might reconsider: " + err.Error() +
				" Do not retry this call and do not attempt the same change by another route. Report the refusal.",
		}, nil
	}

	// An operator may already have answered this exact call, by
	// approving the change set it belongs to (W7). A live grant runs
	// it; a grant that no longer holds is voided and the reason travels
	// with the question below, so the operator is never silently asked
	// twice without being told why.
	grant, stale := g.checkGrant(ctx, t, args)
	if grant != nil {
		return g.spendGrant(ctx, t, key, args, grant)
	}
	if stale != "" {
		g.voidGrant(ctx, t, args, stale)
		g.audit(ctx, t, key, "grant_stale", stale)
	}

	// Park. RequestConfirmation writes the question into the session
	// event log as a long-running function call, which is what makes the
	// pause outlive this process (scoreboard row 5).
	set := g.changeSetContextFor(ctx, t, args)
	if err := ctx.RequestConfirmation(parkHint(key, set, stale), Request{
		Tool:      t.Name(),
		Args:      args,
		Key:       key,
		Policy:    string(g.cfg.Policy),
		Agent:     ctx.AgentName(),
		Verdict:   verdictHelp,
		ChangeSet: set,
		Stale:     stale,
	}); err != nil {
		return nil, fmt.Errorf("approval: requesting confirmation for %s: %w", t.Name(), err)
	}
	g.audit(ctx, t, key, "awaiting_approval", "parked for operator approval")
	if stale != "" {
		return map[string]any{
			"status": "awaiting_operator_approval",
			"detail": "An operator had approved this call as part of a change set, but that approval no longer covers it: " + stale + " " +
				"The call has NOT been made and is parked for a fresh operator decision. " +
				"Do not retry it, do not attempt the same change by another route, and do not treat this as a failure. " +
				"Finish any read-only work, report that the change awaits re-approval, and stop.",
		}, nil
	}
	return map[string]any{
		"status": "awaiting_operator_approval",
		"detail": "This mutating call is parked pending operator approval and has NOT been made. " +
			"It will be made, made with edited arguments, or refused, depending on what the operator decides. " +
			"Do not retry this call, do not attempt the same change by another route, and do not treat this as a failure. " +
			"Finish any read-only work, report that the change awaits approval, and stop.",
	}, nil
}

// parkHint is the one line an operator sees first — in `mast sessions
// show`, in a notification, in a UI's list of pending questions. It
// carries the two things that change what the answer should be: that
// this call is part of a set they can authorize in one go, and that an
// answer they already gave has stopped covering it.
//
// Both belong in the hint rather than only in the payload, because the
// payload is what a client renders when it knows to and the hint is
// what everything renders always.
func parkHint(key string, set *ChangeSetContext, stale string) string {
	hint := fmt.Sprintf("Approve mutating call %s?", key)
	if stale != "" {
		return hint + " (you approved this as part of a change set, and mast is asking again: " + stale + ")"
	}
	if set == nil {
		return hint
	}
	if !set.Grantable {
		return fmt.Sprintf("%s It is 1 of %d calls %s proposed, but they must be approved one at a time: %s",
			hint, len(set.Changes), set.Specialist, set.Ungrantable)
	}
	return fmt.Sprintf("%s It is one of %d calls in the change set %s proposed; answer with scope=change_set to authorize all %d.",
		hint, len(set.Changes), set.Specialist, len(set.Changes))
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
//
// Every path out of here writes a Decision record, including the ones
// that refuse. That is the deferred call at the top and not a line at
// each return, deliberately: this function has nine exits today and will
// grow more, and a feedback dataset that silently omits whichever
// refusal a later change forgot to instrument is worse than no dataset,
// because nothing about it looks wrong (v0.4 W8).
func (g *writeGate) honorVerdict(ctx agent.Context, t tool.Tool, key string, args map[string]any, c *toolconfirmation.ToolConfirmation) (out map[string]any, err error) {
	// Snapshotted here, before anything can rewrite args in place.
	rec := g.newDecision(ctx, t, key, args)
	defer func() { g.recordDecision(ctx, t, &rec, out, err) }()

	v, err := DecodeVerdict(c)
	if err != nil {
		g.audit(ctx, t, key, "malformed_verdict", err.Error())
		return map[string]any{
			"error":  "malformed_verdict",
			"detail": err.Error() + " The call was not made. Report this to the operator; do not retry.",
		}, nil
	}
	rec.Outcome, rec.Scope = v.Verdict, v.Scope
	rec.Approver, rec.Note = v.Approver, v.Note

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
		rec.ExecutedKey, rec.ExecutedArgs = effKey, copyArgs(norm)
	}

	d, err := v.Decision()
	if err != nil {
		g.audit(ctx, t, effKey, "malformed_verdict", err.Error())
		return map[string]any{
			"error":  "malformed_verdict",
			"detail": err.Error() + " The call was not made.",
		}, nil
	}

	// A change-set verdict authorizes more than the call in hand, so it
	// is adjudicated before anything is authorized at all: if the set
	// cannot be granted, neither is this call, and the operator is told
	// to answer one call at a time (W7). Planning only reads — nothing
	// is written until the verdict itself has cleared policy below.
	var pending []Grant
	if v.Verdict != OutcomeReject && v.Scope == ScopeChangeSet {
		p, refusal := g.planGrants(ctx, t, effKey, args, v)
		if refusal != nil {
			return refusal, nil
		}
		pending = p
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

	if v.Scope == ScopeChangeSet {
		g.commitGrants(ctx, t, effKey, v, pending)
	}

	// The capture runs against the arguments that will actually run, so
	// an edited verdict is captured after the edit lands rather than
	// before it — hence this sits below the edit block and not above it.
	// Its refusal path is why the edit is applied to a copy first: a
	// capture that fails must not leave the operator's arguments in the
	// live map of a call that did not run.
	running := args
	if edited != nil {
		running = edited
	}
	if refusal := g.permit(ctx, t, effKey, running); refusal != nil {
		return refusal, nil
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

// permit is the last thing that happens before a mutating call is
// allowed to run, on every path that allows one: policy-is-apply, an
// operator's approval, an operator's edit, and a change-set grant. A
// non-nil return is a refusal to hand the model; nil means proceed.
//
// One function rather than a line at each of the four, for the reason
// runOwnBehalf gives about its own three callers: the count is the
// point. "Everything mast overwrote was recorded first" is a claim about
// *every* path, and a fifth path that forgets to call this is the only
// way the claim becomes false — so the paths are here, together, where
// the next one added is visibly next to them.
//
// It does nothing at all unless a tool declares a capture. When one
// does, it is fail-closed: see capture.go for why refusing is the
// honest reading of the declaration.
func (g *writeGate) permit(ctx agent.Context, t tool.Tool, key string, args map[string]any) map[string]any {
	decl, err := g.cfg.Captures.declared(t.Name())
	if err != nil {
		g.audit(ctx, t, key, "capture_undeclarable", err.Error())
		return map[string]any{
			"error": "capture_failed",
			"detail": "This workload's declaration of what to record before changing " + t.Name() + " could not be read (" + err.Error() + "), " +
				"so mast did not make the change. Nothing happened. Report this and stop.",
		}
	}
	if decl == nil {
		return nil
	}
	rec, err := g.cfg.Captures.take(ctx, t.Name(), key, args, decl)
	if err != nil {
		g.audit(ctx, t, key, "capture_failed", err.Error())
		g.cfg.Logger.Warn("write gate: refusing an authorized mutating call because its prior state could not be captured",
			"tool", t.Name(), "call", key, "error", err.Error(),
			"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
			"function_call_id", ctx.FunctionCallID())
		return map[string]any{
			"error": "capture_failed",
			"detail": "This call was authorized, but mast could not first record what it would overwrite: " + err.Error() + ". " +
				"The call was NOT made and nothing changed. This workload declares that " + t.Name() + " must not run without a prior-state record, " +
				"so this is a refusal and not a retryable error. Report it and stop.",
		}
	}
	g.recordCapture(ctx, t, rec)
	return nil
}

// recordCapture persists the prior-state record on the state delta of
// the event ADK is already appending for this call — one writer, the
// same discipline writeDecision, recordEdit and writeGrant follow (mast
// #45/#46).
//
// Unlike those three, a failure here refuses the call. They are audit
// records written after an adjudication that has already happened, so
// dropping one costs a dataset a row; this one is the thing the call was
// permitted on the strength of, and a call that runs after its capture
// failed to persist is exactly the un-undoable effect the record exists
// to prevent. The window that cannot be closed is the same one the
// effects outbox names: the delta lands with the event, so a crash
// between the tool firing and the event persisting loses the capture
// along with the record that the call happened at all.
func (g *writeGate) recordCapture(ctx agent.Context, t tool.Tool, rec *CaptureRecord) {
	if rec == nil || ctx == nil {
		return
	}
	rec.Workload = g.cfg.Workload
	raw, err := EncodeCapture(*rec)
	if err != nil {
		g.cfg.Logger.Error("write gate: captured prior state but could not encode its record",
			"tool", t.Name(), "call", rec.Key, "error", err.Error())
		return
	}
	a := ctx.Actions()
	if a == nil {
		g.cfg.Logger.Error("write gate: no event actions to record captured prior state on",
			"tool", t.Name(), "call", rec.Key)
		return
	}
	if a.StateDelta == nil {
		a.StateDelta = map[string]any{}
	}
	a.StateDelta[CaptureStateKey(ctx.FunctionCallID())] = raw
	g.cfg.Logger.Info("write gate: prior state captured before a mutating call",
		"tool", t.Name(), "call", rec.Key, "read", CallKey(rec.Read, rec.ReadArgs),
		"digest", rec.Digest, "undoable", rec.Undoable(), "revert", revertKey(rec),
		"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
		"function_call_id", ctx.FunctionCallID())
}

func revertKey(rec *CaptureRecord) string {
	if rec == nil || rec.Revert == nil {
		return ""
	}
	return CallKey(rec.Revert.Tool, rec.Revert.Arguments)
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

// newDecision opens the record for one adjudication, capturing the call
// as proposed before anything downstream can rewrite it in place.
func (g *writeGate) newDecision(ctx agent.Context, t tool.Tool, key string, args map[string]any) Decision {
	d := Decision{
		DecidedAt:    time.Now().UTC(),
		Workload:     g.cfg.Workload,
		Tool:         t.Name(),
		Authority:    AuthorityVerdict,
		ProposedKey:  key,
		ProposedArgs: copyArgs(args),
	}
	if ctx != nil {
		d.Session = ctx.SessionID()
		d.Specialist = ctx.AgentName()
		d.Invocation = ctx.InvocationID()
		d.FunctionCallID = ctx.FunctionCallID()
	}
	return d
}

// recordDecision closes the record and writes it, deriving what the gate
// did from what the gate returned.
//
// Reading the disposition off the response rather than tracking it at
// each exit is what keeps the two honest: the response map IS what the
// model was told, so a record derived from it cannot claim the call was
// authorized while the model was told it was refused.
//
// A reject is attributed to the operator regardless of the code the
// model saw, because the operator's answer is the cause; everything else
// that produced a response is mast declining to honor a verdict.
func (g *writeGate) recordDecision(ctx agent.Context, t tool.Tool, d *Decision, out map[string]any, err error) {
	switch {
	case err != nil:
		// No path returns one today. If one ever does, the call did not
		// run and the dataset should say why rather than skip the row.
		d.Disposition, d.Refusal = DispositionRefusedByMast, "gate_error"
	case out == nil:
		d.Disposition = DispositionAuthorized
	case d.Outcome == OutcomeReject:
		d.Disposition, d.Refusal = DispositionRefusedByOperator, refusalCode(out)
	default:
		d.Disposition, d.Refusal = DispositionRefusedByMast, refusalCode(out)
	}
	g.writeDecision(ctx, t, *d)
}

func refusalCode(out map[string]any) string {
	code, _ := out["error"].(string)
	return code
}

// writeDecision persists one Decision on the state delta of the event
// ADK is already appending for this tool call — the same discipline
// recordEdit and writeGrant follow, and for the same reason: the runner
// owns that row's write lease, so a second writer would invalidate its
// handle and kill the turn (mast #45/#46).
//
// This is why the record does NOT ride the `:mast-ops` companion row the
// pause and abort markers use. That mechanism exists for writers
// OUTSIDE the turn (an operator's `mast sessions abort` landing while a
// runner holds the lease). The write gate is inside the turn; using the
// ops row here would need a transcript.Store the gate has no way to
// obtain — pkg/transcript imports pkg/approval, so the dependency cannot
// run the other way — and would trade a safe write for a racy one.
//
// A record that cannot be encoded is logged and dropped. The call has
// already been adjudicated at this point; failing it to protect a
// dataset would let the feedback loop veto the cluster.
func (g *writeGate) writeDecision(ctx agent.Context, t tool.Tool, d Decision) {
	if ctx == nil {
		return
	}
	raw, err := EncodeDecision(d)
	if err != nil {
		g.cfg.Logger.Error("write gate: adjudicated a call but could not encode its decision record",
			"tool", t.Name(), "call", d.ProposedKey, "error", err.Error())
		return
	}
	a := ctx.Actions()
	if a == nil {
		return
	}
	if a.StateDelta == nil {
		a.StateDelta = map[string]any{}
	}
	a.StateDelta[DecisionStateKey(ctx.FunctionCallID())] = raw
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

	// ChangeSet describes the approved-as-a-unit set this call belongs
	// to, when it belongs to one (W7). Present means `scope:
	// change_set` is on the table: the operator is being asked about
	// one call and can authorize the rest with the same answer, and
	// they can only make that trade if they are shown what the rest
	// are.
	ChangeSet *ChangeSetContext `json:"change_set,omitempty"`

	// Stale is why an approval this operator already gave no longer
	// covers this call. Its presence is the difference between "please
	// approve this" and "you approved this, and mast is asking again
	// because the ground moved" — which is the whole point of checking
	// freshness rather than trusting a clock.
	Stale string `json:"stale,omitempty"`
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
	"scope":   "once (default) | change_set (only when this question carries a change_set: authorizes every call listed there, each bound to its exact arguments)",
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
