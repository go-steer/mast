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

package permissions

import (
	"context"
	"errors"
	"fmt"
)

// ErrApprovalRequired reports that policy has no objection to a mutating
// tool call but that the call may not proceed until an operator approves
// it. It is the "ask a human" answer, not a denial, and it is the only
// non-denial answer CheckMutatingToolCall gives.
//
// The caller owns the asking. This package deliberately does not: a
// permissions.Prompter is a synchronous in-process question and cannot
// survive the process dying while an operator thinks about it, which is
// exactly what a mutation approval has to do (docs/v0.3-plan.md
// scoreboard row 5). mast's write gate parks the call as an ADK tool
// confirmation in the durable session event log instead. A caller with
// nowhere to park must treat this as a denial.
var ErrApprovalRequired = errors.New("permissions: mutating tool call requires operator approval")

// ErrGrantScopeRefused reports that an operator's verdict on a mutating
// tool call came back with a grant scope broader than "this one call".
// See RecordMutationVerdict.
var ErrGrantScopeRefused = errors.New("permissions: grant scope not admissible for a mutating tool")

// CheckMutatingToolCall applies policy to a call the workload's mutation
// predicate classifies as mutating (docs/orchestration-design.md,
// hitl_policy.on_mutation; docs/v0.3-plan.md W2.1/W2.3).
//
// It never returns nil. Either the call is refused outright — the error
// says why — or the answer is ErrApprovalRequired, meaning "policy is
// satisfied, now go ask a human". No mutation is ever auto-approved
// here, which is the whole difference between this entry point and
// CheckToolCall.
//
// That difference is deliberate and total. Every auto-approval path the
// ordinary gate offers is developer-tool ergonomics, and that is the
// wrong risk model for cluster mutation, so none of them apply:
//
//   - Mode does not short-circuit. ModeYolo, ModeAllow and
//     ModeAcceptEdits all still require the approval. An unattended
//     triage daemon runs permissively because otherwise every read would
//     block; it must still stop before it writes, and W2.3 makes that
//     non-bypassable rather than a setting an operator has to get right.
//   - Session grants are neither read nor written. "Allow every call to
//     this tool for the session" applied to patch_resource hands over
//     the namespace for the session.
//   - A policy allow is not an approval. Deny still denies — deny is
//     always maximal — but OutcomeAllow only means "not on the deny
//     list", and it does not skip the operator.
//
// What does still apply, in order: the plan-first pre-check, the deny
// policy, and ModePlan, under which nothing executes at all.
//
// key is the human-readable detail an operator will be shown — a tool
// call plus a summary of its arguments — and is also what the deny
// policy matches against, so it must describe the call being made and
// not merely name the tool.
func (g *Gate) CheckMutatingToolCall(ctx context.Context, toolName, key string) error {
	g = g.resolveSessionGate(ctx)
	if err := g.planFirstDenial(toolName); err != nil {
		return err
	}
	if g.policy.Match(toolName, key) == OutcomeDeny {
		return fmt.Errorf("%s denied by config policy: %q", toolName, key)
	}
	if g.Mode() == ModePlan {
		return fmt.Errorf("%s denied: tool execution disabled in 'plan' mode (%q)", toolName, key)
	}
	return fmt.Errorf("%w (tool=%s detail=%q)", ErrApprovalRequired, toolName, key)
}

// RecordMutationVerdict closes the loop CheckMutatingToolCall opened: an
// operator has answered, and this decides whether that answer authorizes
// the call.
//
// The admissible answers are exactly DecisionAllowOnce and
// DecisionDeny. Anything broader is refused with ErrGrantScopeRefused
// rather than silently narrowed to allow-once, so a UI that offers the
// wrong buttons fails loudly instead of granting more than the operator
// was told they were granting (W2.3). The refusal is not a denial of the
// operator's intent — it is a refusal of the *scope*; the call does not
// proceed and the next attempt asks again.
//
// The second admissible grant form named in W2.3 — change-set-minted and
// bound to an exact normalized (tool, arguments) signature — is consumed
// by the change-set path in front of this gate (W7) and is not a
// decision made here.
//
// Nothing is remembered on any path. An allow-once is recorded in the
// approval audit log and nowhere else, so the next mutating call asks
// again even for a byte-identical request.
func (g *Gate) RecordMutationVerdict(ctx context.Context, toolName, key string, d Decision) error {
	g = g.resolveSessionGate(ctx)
	switch d {
	case DecisionDeny:
		return fmt.Errorf("%s denied by operator: %q", toolName, key)
	case DecisionAllowOnce:
		g.recordApproval(toolName, key, d)
		return nil
	default:
		return fmt.Errorf("%w: %s for mutating tool %s (%q); only allow-once is admissible for a mutation", ErrGrantScopeRefused, d, toolName, key)
	}
}
