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
	"strings"
	"testing"
)

const (
	mutTool = "scale_deployment"
	mutKey  = "scale_deployment(deployment=api, replicas=10)"
)

// alwaysPrompter answers every request with a fixed decision and
// records that it was consulted. CheckMutatingToolCall must never
// consult it: the pause for a mutation is durable and lives outside
// this package.
type alwaysPrompter struct {
	d     Decision
	calls int
}

func (p *alwaysPrompter) AskApproval(context.Context, PromptRequest) (Decision, error) {
	p.calls++
	return p.d, nil
}

// TestCheckMutatingToolCall_NeverAutoApproves is W2.3's core claim: no
// mode, grant, or allowlist entry lets a mutating call through without
// an operator. Each subtest configures the most permissive version of
// one bypass and asserts the answer is still "ask a human".
func TestCheckMutatingToolCall_NeverAutoApproves(t *testing.T) {
	t.Parallel()
	allowEverything, err := NewPolicy([]string{"*"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		build func() *Gate
	}{
		{"yolo mode", func() *Gate {
			return New(Options{Mode: ModeYolo})
		}},
		{"allow mode", func() *Gate {
			return New(Options{Mode: ModeAllow})
		}},
		{"accept-edits mode", func() *Gate {
			return New(Options{Mode: ModeAcceptEdits})
		}},
		{"policy allowlist matches", func() *Gate {
			return New(Options{Mode: ModeAsk, Policy: allowEverything})
		}},
		{"session grant for this exact request", func() *Gate {
			g := New(Options{Mode: ModeAsk})
			g.rememberSession(mutTool, mutKey)
			return g
		}},
		{"session grant for the whole tool", func() *Gate {
			g := New(Options{Mode: ModeAsk})
			g.rememberSessionTool(mutTool)
			return g
		}},
		{"yolo mode and a wired prompter that says yes", func() *Gate {
			return New(Options{Mode: ModeYolo, Prompter: &alwaysPrompter{d: DecisionAllowAlways}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.build().CheckMutatingToolCall(context.Background(), mutTool, mutKey)
			if !errors.Is(err, ErrApprovalRequired) {
				t.Fatalf("CheckMutatingToolCall = %v, want ErrApprovalRequired", err)
			}
		})
	}
}

// TestCheckMutatingToolCall_NeverPrompts pins the division of labour
// this package's half of W2 rests on: pkg/permissions decides policy
// and never asks. A synchronous in-process prompt cannot survive the
// operator taking a coffee break while the process is restarted, which
// is precisely what a mutation approval has to do (scoreboard row 5),
// so the ask belongs to the durable seam in pkg/approval.
func TestCheckMutatingToolCall_NeverPrompts(t *testing.T) {
	t.Parallel()
	p := &alwaysPrompter{d: DecisionAllowOnce}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	if err := g.CheckMutatingToolCall(context.Background(), mutTool, mutKey); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("CheckMutatingToolCall = %v, want ErrApprovalRequired", err)
	}
	if p.calls != 0 {
		t.Errorf("prompter consulted %d time(s), want 0: the mutation pause is durable and does not run through Prompter", p.calls)
	}
}

// TestCheckMutatingToolCall_RefusalsStillApply covers the three checks
// that DO survive into the mutation gate. Each must beat the
// ask-a-human answer, so the assertion is that the error is NOT
// ErrApprovalRequired.
func TestCheckMutatingToolCall_RefusalsStillApply(t *testing.T) {
	t.Parallel()
	denyScale, err := NewPolicy(nil, []string{mutTool + ":*"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		gate *Gate
		want string
	}{
		{
			// Deny is maximal everywhere else in this package and stays
			// maximal here — a bundle that says "just apply mutations"
			// must not outrank an operator's configured deny rule.
			name: "config deny beats yolo mode",
			gate: New(Options{Mode: ModeYolo, Policy: denyScale}),
			want: "denied by config policy",
		},
		{
			name: "plan mode executes nothing",
			gate: New(Options{Mode: ModePlan}),
			want: "'plan' mode",
		},
		{
			name: "plan-first pre-check runs before anything else",
			gate: New(Options{Mode: ModeYolo, RequirePlanArtifact: true}),
			want: "plan-first mode requires record_plan",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.gate.CheckMutatingToolCall(context.Background(), mutTool, mutKey)
			if err == nil {
				t.Fatal("CheckMutatingToolCall = nil, want a refusal")
			}
			if errors.Is(err, ErrApprovalRequired) {
				t.Fatalf("CheckMutatingToolCall = %v, want a refusal, not an approval request", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// TestRecordMutationVerdict_ScopesRefused is the assertion U-gate-scopes
// names: only allow-once passes. A prompter that offers "for the
// session" or "always" on a mutation is offering something the gate
// will not honour, and it finds out by being refused rather than by
// having its answer quietly reinterpreted.
func TestRecordMutationVerdict_ScopesRefused(t *testing.T) {
	t.Parallel()
	broad := []Decision{
		DecisionAllowSession,
		DecisionAllowSessionVerb,
		DecisionAllowSessionTool,
		DecisionAllowAlways,
	}
	for _, d := range broad {
		t.Run(d.String(), func(t *testing.T) {
			t.Parallel()
			g := New(Options{Mode: ModeYolo})
			err := g.RecordMutationVerdict(context.Background(), mutTool, mutKey, d)
			if !errors.Is(err, ErrGrantScopeRefused) {
				t.Fatalf("RecordMutationVerdict(%s) = %v, want ErrGrantScopeRefused", d, err)
			}
			// A refused scope is not an approval and must not read as
			// one in the audit log.
			if got := g.Approvals(); len(got) != 0 {
				t.Errorf("approval log has %d entr(ies) after a refused scope, want 0: %+v", len(got), got)
			}
			// Nor may it leave a standing grant behind that a later
			// call could ride through the ordinary gate.
			if g.sessionToolAllowed(mutTool) || g.sessionAllowed(mutTool, mutKey) {
				t.Error("a refused scope installed a session grant")
			}
		})
	}
}

func TestRecordMutationVerdict_AllowOnce(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeAsk})
	if err := g.RecordMutationVerdict(context.Background(), mutTool, mutKey, DecisionAllowOnce); err != nil {
		t.Fatalf("RecordMutationVerdict(allow-once) = %v, want nil", err)
	}
	log := g.Approvals()
	if len(log) != 1 || log[0].Tool != mutTool || log[0].Key != mutKey || log[0].Decision != DecisionAllowOnce {
		t.Fatalf("approval log = %+v, want one allow-once for %s/%s", log, mutTool, mutKey)
	}
	// Recorded for audit, remembered for nothing: the identical call
	// asks again.
	if err := g.CheckMutatingToolCall(context.Background(), mutTool, mutKey); !errors.Is(err, ErrApprovalRequired) {
		t.Errorf("second CheckMutatingToolCall = %v, want ErrApprovalRequired — an allow-once must not install a standing grant", err)
	}
}

func TestRecordMutationVerdict_Deny(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo})
	err := g.RecordMutationVerdict(context.Background(), mutTool, mutKey, DecisionDeny)
	if err == nil {
		t.Fatal("RecordMutationVerdict(deny) = nil, want a denial")
	}
	if errors.Is(err, ErrGrantScopeRefused) {
		t.Fatalf("deny reported as a scope refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "denied by operator") {
		t.Errorf("error %q does not attribute the denial to the operator", err)
	}
}

// TestMutationGate_RoutesThroughTheSessionGate pins that both halves
// resolve the per-session sub-gate the way every other public Check*
// entry point does. Without it a daemon's per-session policy would be
// silently ignored for exactly the calls that matter most.
func TestMutationGate_RoutesThroughTheSessionGate(t *testing.T) {
	t.Parallel()
	denyScale, err := NewPolicy(nil, []string{mutTool + ":*"})
	if err != nil {
		t.Fatal(err)
	}
	template := New(Options{Mode: ModeAsk})
	session := New(Options{Mode: ModeAsk, Policy: denyScale})
	ctx := WithSessionGate(context.Background(), session)

	if err := template.CheckMutatingToolCall(ctx, mutTool, mutKey); !strings.Contains(errString(err), "denied by config policy") {
		t.Errorf("CheckMutatingToolCall on the template gate = %v, want the session gate's deny", err)
	}
	if err := template.RecordMutationVerdict(ctx, mutTool, mutKey, DecisionAllowOnce); err != nil {
		t.Fatalf("RecordMutationVerdict = %v", err)
	}
	if len(session.Approvals()) != 1 {
		t.Error("approval recorded on the template gate, not the session gate")
	}
	if len(template.Approvals()) != 0 {
		t.Error("approval recorded on the template gate")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
