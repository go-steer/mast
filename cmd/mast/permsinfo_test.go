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

package main

import (
	"context"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/permissions"
	"github.com/go-steer/mast/pkg/transcript"
)

// The GET /perms projection (#375). TestAttachWiringLeavesNoCapability
// Unwired proves PermsFn is non-nil; it cannot tell a correct answer
// from a well-formed wrong one, which is the exact failure this issue
// is about.

// seedDecisions creates a session whose event log carries the given
// adjudications, in order, and returns a wiring that reads them back
// through the real transcript store.
func seedDecisions(t *testing.T, sid string, decisions ...approval.Decision) attachWiring {
	t.Helper()
	svc := adksession.InMemoryService()
	ctx := context.Background()
	resp, err := svc.Create(ctx, &adksession.CreateRequest{AppName: appName, UserID: "operator", SessionID: sid})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	base := time.Now()
	for i, d := range decisions {
		raw, err := approval.EncodeDecision(d)
		if err != nil {
			t.Fatalf("encode decision %d: %v", i, err)
		}
		ev := adksession.NewEvent(ctx, "inv-1")
		ev.Author = "mast"
		ev.Timestamp = base.Add(time.Duration(i+1) * time.Second)
		ev.Actions.StateDelta = map[string]any{approval.DecisionStateKey(d.FunctionCallID): raw}
		if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}
	return attachWiring{store: transcript.NewStore(svc, appName)}
}

// TestPermsReportsTheGateOrNothing: mode, allow and deny come from the
// gate compose actually built, and are absent when it built none.
//
// The alternative that was rejected is reporting "ask" unconditionally
// — mast's gate is always in ask mode when it exists, so the constant
// would pass every test that has a gate and be a lie on every daemon
// running apply or dry_run, where nothing consults a mode at all.
func TestPermsReportsTheGateOrNothing(t *testing.T) {
	t.Parallel()

	policy, err := permissions.NewPolicy([]string{"tool.read_file"}, []string{"tool.bash:rm *"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	gate := permissions.New(permissions.Options{Policy: policy})
	w := attachWiring{gate: gate, onMutation: "require_approval"}
	got := w.perms("s1")
	if got.Mode != string(gate.Mode()) {
		t.Errorf("mode = %q, want the gate's own %q", got.Mode, gate.Mode())
	}
	if len(got.Allow) != 1 || len(got.Deny) != 1 {
		t.Errorf("allow/deny = %v / %v, want the gate's patterns", got.Allow, got.Deny)
	}
	if got.OnMutation != "require_approval" {
		t.Errorf("on_mutation = %q", got.OnMutation)
	}

	// apply and dry_run build no gate, and the answer must say so by
	// omission rather than by naming a mode.
	for _, policy := range []string{"apply", "dry_run"} {
		got := attachWiring{onMutation: policy}.perms("s1")
		if got.Mode != "" {
			t.Errorf("%s: mode = %q, want empty — there is no permissions gate to describe", policy, got.Mode)
		}
		if got.OnMutation != policy {
			t.Errorf("%s: on_mutation = %q, want the policy carried on its own axis", policy, got.OnMutation)
		}
		if got.Allow != nil || got.Deny != nil {
			t.Errorf("%s: allow/deny non-nil with no gate: %v / %v", policy, got.Allow, got.Deny)
		}
	}
}

// TestPermsDecisionIsDrivenByDisposition is the mapping this projection
// gets to choose, and the reason it chose Disposition over Outcome.
//
// Outcome is what the operator answered. Disposition is what the gate
// did. They come apart in two recorded ways, and both are rows here: a
// verdict mast refuses (Outcome says approve, the call did not run),
// and a call that fired under an earlier change-set grant (no Outcome
// at all, because nobody was asked about this call).
func TestPermsDecisionIsDrivenByDisposition(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in           approval.Decision
		wantDecision string
		wantRefusal  string
		wantSet      string
	}{
		"an operator approved it": {
			in: approval.Decision{
				Tool: "scale", Outcome: approval.OutcomeApprove,
				Disposition: approval.DispositionAuthorized,
			},
			wantDecision: "allow-once",
		},
		"an operator refused it": {
			in: approval.Decision{
				Tool: "scale", Outcome: approval.OutcomeReject,
				Disposition: approval.DispositionRefusedByOperator,
				Refusal:     "denied_by_operator",
			},
			wantDecision: "deny",
			wantRefusal:  "denied_by_operator",
		},
		"mast refused the operator's answer": {
			// Outcome is approve and the call did not run. A row keyed
			// on Outcome would read "allow-once" about a refusal.
			in: approval.Decision{
				Tool: "scale", Outcome: approval.OutcomeApprove,
				Disposition: approval.DispositionRefusedByMast,
				Refusal:     "edit_refused",
			},
			wantDecision: "deny",
			wantRefusal:  "edit_refused",
		},
		"it fired under an earlier grant": {
			// No Outcome: nobody was asked about THIS call. Without
			// change_set a client would present it as individually
			// approved, which is a false account of what the operator
			// authorized.
			in: approval.Decision{
				Tool: "scale", Authority: approval.AuthorityChangeSetGrant,
				Disposition: approval.DispositionAuthorized,
				ChangeSet:   "cs-7",
			},
			wantDecision: "allow-once",
			wantSet:      "cs-7",
		},
		"a disposition this build does not know": {
			in: approval.Decision{
				Tool: "scale", Disposition: approval.Disposition("quarantined"),
			},
			wantDecision: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := approvalInfo(tc.in)
			if got.Decision != tc.wantDecision {
				t.Errorf("decision = %q, want %q", got.Decision, tc.wantDecision)
			}
			if got.Refusal != tc.wantRefusal {
				t.Errorf("refusal = %q, want %q", got.Refusal, tc.wantRefusal)
			}
			if got.ChangeSet != tc.wantSet {
				t.Errorf("change_set = %q, want %q", got.ChangeSet, tc.wantSet)
			}
		})
	}
}

// TestPermsKeyIsWhatRan: on an edit the row describes the call that
// executed, not the one the operator specifically rejected.
func TestPermsKeyIsWhatRan(t *testing.T) {
	t.Parallel()
	edited := approval.Decision{
		Tool:        "scale_deployment",
		Outcome:     approval.OutcomeEdit,
		Disposition: approval.DispositionAuthorized,
		ProposedKey: "scale_deployment:api:10",
		ExecutedKey: "scale_deployment:api:3",
		Approver:    "alice@example.com",
	}
	got := approvalInfo(edited)
	if got.Key != "scale_deployment:api:3" {
		t.Errorf("key = %q, want the executed call — the proposal is the thing a human overruled", got.Key)
	}
	if got.Approver != "alice@example.com" {
		t.Errorf("approver = %q, want the recorded identity (#194)", got.Approver)
	}

	unedited := approval.Decision{
		Tool:        "scale_deployment",
		Disposition: approval.DispositionAuthorized,
		ProposedKey: "scale_deployment:api:10",
	}
	if got := approvalInfo(unedited).Key; got != "scale_deployment:api:10" {
		t.Errorf("key = %q, want the proposal when nothing was edited", got)
	}
}

// TestPermsReadsTheDurableLog end-to-ends the approvals half through a
// real store, and pins the ordering.
//
// Deliberately not gate.Approvals(): that log is in-memory, daemon-wide
// and carries no session id, so on a multi-session daemon it would
// report every session's answers under whichever one was asked.
func TestPermsReadsTheDurableLog(t *testing.T) {
	t.Parallel()
	w := seedDecisions(t, "s-log",
		approval.Decision{
			FunctionCallID: "fc-1", Tool: "restart", ProposedKey: "restart:api",
			Disposition: approval.DispositionAuthorized, Approver: "alice@example.com",
		},
		approval.Decision{
			FunctionCallID: "fc-2", Tool: "delete", ProposedKey: "delete:db",
			Disposition: approval.DispositionRefusedByOperator, Refusal: "denied_by_operator",
		},
	)
	w.onMutation = "require_approval"

	got := w.perms("s-log")
	if len(got.Approvals) != 2 {
		t.Fatalf("approvals = %+v, want 2 rows read back from the event log", got.Approvals)
	}
	if got.Approvals[0].Tool != "restart" || got.Approvals[1].Tool != "delete" {
		t.Errorf("approvals out of order: %+v — the sequence is the dataset", got.Approvals)
	}
	if got.Approvals[0].Decision != "allow-once" || got.Approvals[1].Decision != "deny" {
		t.Errorf("decisions = %q / %q", got.Approvals[0].Decision, got.Approvals[1].Decision)
	}

	// A session on this daemon that decided nothing reports nothing,
	// rather than borrowing the other session's rows.
	if other := w.perms("s-does-not-exist"); len(other.Approvals) != 0 {
		t.Errorf("unknown session carried %d approvals", len(other.Approvals))
	}
}

// TestPermsDegradesToTheRulesHalf: an unreadable store costs the
// approvals, not the request. Same posture as turnState — the rules
// half is still true, and an operator reaches for this surface
// precisely when the daemon is unhappy.
func TestPermsDegradesToTheRulesHalf(t *testing.T) {
	t.Parallel()
	w := attachWiring{gate: permissions.New(permissions.Options{}), onMutation: "require_approval"}
	got := w.perms("s-nostore")
	if got.Mode == "" {
		t.Error("mode empty with a gate present; the rules half does not depend on the store")
	}
	if len(got.Approvals) != 0 {
		t.Errorf("approvals = %+v with no store", got.Approvals)
	}
}
