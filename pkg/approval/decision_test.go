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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// decisions returns every Decision record in the session state, which is
// where the write gate wrote them — the same place `mast sessions
// export-decisions` reads from, one projection later.
func (p *gateProbe) decisions(t *testing.T) []Decision {
	t.Helper()
	var out []Decision
	for k, v := range p.state {
		if !strings.HasPrefix(k, DecisionStateKeyPrefix) {
			continue
		}
		d, err := DecodeDecision(v)
		if err != nil {
			t.Fatalf("state[%q] is not a decision record: %v", k, err)
		}
		out = append(out, d)
	}
	return out
}

// onlyDecision fails unless the run produced exactly one record.
func (p *gateProbe) onlyDecision(t *testing.T) Decision {
	t.Helper()
	got := p.decisions(t)
	if len(got) != 1 {
		t.Fatalf("decision records = %+v, want exactly one", got)
	}
	return got[0]
}

func wantDecision(t *testing.T, got Decision, want Decision) {
	t.Helper()
	if got.Outcome != want.Outcome {
		t.Errorf("Outcome = %q, want %q", got.Outcome, want.Outcome)
	}
	if got.Disposition != want.Disposition {
		t.Errorf("Disposition = %q, want %q", got.Disposition, want.Disposition)
	}
	if got.Refusal != want.Refusal {
		t.Errorf("Refusal = %q, want %q", got.Refusal, want.Refusal)
	}
	if got.Authority != want.Authority {
		t.Errorf("Authority = %q, want %q", got.Authority, want.Authority)
	}
	if got.Approver != want.Approver {
		t.Errorf("Approver = %q, want %q", got.Approver, want.Approver)
	}
}

// TestDecisionRecordedOnApprove: the answer that leaves the least trace
// behind. An approved call is indistinguishable in the transcript from
// one no operator ever saw, so without this record the exported dataset
// would contain only refusals and edits — every label except the
// commonest one.
func TestDecisionRecordedOnApprove(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:  OnMutationRequireApproval,
		respond: approve(map[string]any{"verdict": "approve", "note": "expected", "approver": "user:sre-oncall"}),
	})
	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
	d := p.onlyDecision(t)
	wantDecision(t, d, Decision{
		Outcome:     OutcomeApprove,
		Disposition: DispositionAuthorized,
		Authority:   AuthorityVerdict,
		Approver:    "user:sre-oncall",
	})
	if d.Tool != "scale_deployment" {
		t.Errorf("Tool = %q, want scale_deployment", d.Tool)
	}
	if d.Note != "expected" {
		t.Errorf("Note = %q, want the operator's note", d.Note)
	}
	if d.Session != sid {
		t.Errorf("Session = %q, want %q — a row that cannot be traced back to its session is not evidence", d.Session, sid)
	}
	if d.DecidedAt.IsZero() {
		t.Error("DecidedAt is zero; an undated decision cannot be windowed or ordered")
	}
	if d.ProposedKey == "" || d.ProposedArgs["deployment"] != "api" {
		t.Errorf("ProposedKey/ProposedArgs = %q/%v, want the model's call", d.ProposedKey, d.ProposedArgs)
	}
	if d.Edited() {
		t.Errorf("Edited() = true on a plain approval (executed key %q)", d.ExecutedKey)
	}
}

// TestDecisionRecordedOnReject: the record the workstream would be
// worthless without.
//
// A refusal leaves no FunctionCall the tool ever saw and no applied-edit
// record — only an error string in the model's response, which looks
// exactly like a tool that failed. If rejects were uncapturable the
// export would be all approvals, and a dataset of nothing but yeses
// teaches the wrong thing about operator judgement.
//
// It also pins the ADK behaviour the capture depends on: a before-tool
// callback that short-circuits the call with a response map still has
// its Actions — and so its StateDelta — assigned onto the event ADK
// appends (v2.2.0 llminternal.base_flow).
func TestDecisionRecordedOnReject(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: func(confID string) *genai.Content {
			return verdictResponse(confID, map[string]any{
				"confirmed": false,
				"payload": map[string]any{
					"verdict":  "reject",
					"note":     "we are not scaling into an OOM",
					"approver": "user:sre-oncall",
				},
			})
		},
	})
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) after a rejection: %+v", len(p.executions), p.executions)
	}
	d := p.onlyDecision(t)
	wantDecision(t, d, Decision{
		Outcome:     OutcomeReject,
		Disposition: DispositionRefusedByOperator,
		Refusal:     "denied_by_operator",
		Authority:   AuthorityVerdict,
		Approver:    "user:sre-oncall",
	})
	if d.Note != "we are not scaling into an OOM" {
		t.Errorf("Note = %q, want the operator's reason — the reason IS the label", d.Note)
	}
	if d.ProposedArgs["deployment"] != "api" {
		t.Errorf("ProposedArgs = %v, want the call that was refused", d.ProposedArgs)
	}
}

// TestDecisionRecordedOnEdit: the densest row in the dataset — a human
// writing down what the model should have said. Both halves have to be
// in the one record, because a proposal without its correction is not a
// label.
func TestDecisionRecordedOnEdit(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: approve(map[string]any{
			"verdict":  "edit",
			"args":     map[string]any{"deployment": "api", "replicas": 2},
			"note":     "10 would exhaust the node pool",
			"approver": "user:sre-oncall",
		}),
	})
	if len(p.executions) != 1 || p.executions[0].Replicas != 2 {
		t.Fatalf("executions = %+v, want one call with the operator's 2 replicas", p.executions)
	}
	d := p.onlyDecision(t)
	wantDecision(t, d, Decision{
		Outcome:     OutcomeEdit,
		Disposition: DispositionAuthorized,
		Authority:   AuthorityVerdict,
		Approver:    "user:sre-oncall",
	})
	if !d.Edited() {
		t.Fatalf("Edited() = false on an edit verdict: proposed %q, executed %q", d.ProposedKey, d.ExecutedKey)
	}
	if !strings.Contains(d.ProposedKey, "replicas=10") {
		t.Errorf("ProposedKey = %q, want the model's 10 replicas", d.ProposedKey)
	}
	if !strings.Contains(d.ExecutedKey, "replicas=2") {
		t.Errorf("ExecutedKey = %q, want the operator's 2 replicas", d.ExecutedKey)
	}
	if got := d.ProposedArgs["replicas"]; got != float64(10) {
		t.Errorf("ProposedArgs[replicas] = %v (%T), want the model's 10 — the proposal must survive applyArgs rewriting the live map", got, got)
	}
	if got := d.ExecutedArgs["replicas"]; got != float64(2) {
		t.Errorf("ExecutedArgs[replicas] = %v (%T), want the operator's 2", got, got)
	}
}

// TestDecisionRecordsMastsOwnRefusals: an edit mast refused is a
// different row from an edit an operator never made, and a dataset that
// silently dropped it would over-report how often mast honors what
// operators ask for.
func TestDecisionRecordsMastsOwnRefusals(t *testing.T) {
	t.Run("unattributed edit", func(t *testing.T) {
		p := runGateProbe(t, gateProbeConfig{
			policy: OnMutationRequireApproval,
			respond: approve(map[string]any{
				"verdict": "edit",
				"args":    map[string]any{"deployment": "api", "replicas": 2},
			}),
		})
		wantDecision(t, p.onlyDecision(t), Decision{
			Outcome:     OutcomeEdit,
			Disposition: DispositionRefusedByMast,
			Refusal:     "edit_unattributed",
			Authority:   AuthorityVerdict,
		})
	})

	t.Run("malformed verdict", func(t *testing.T) {
		// No readable outcome at all. The row is still written, because
		// "clients keep sending payloads mast cannot read" is a finding.
		p := runGateProbe(t, gateProbeConfig{
			policy: OnMutationRequireApproval,
			respond: func(confID string) *genai.Content {
				return verdictResponse(confID, map[string]any{
					"confirmed": false,
					"payload":   map[string]any{"verdict": "approve"},
				})
			},
		})
		d := p.onlyDecision(t)
		wantDecision(t, d, Decision{
			Disposition: DispositionRefusedByMast,
			Refusal:     "malformed_verdict",
			Authority:   AuthorityVerdict,
		})
		if d.Outcome != "" {
			t.Errorf("Outcome = %q, want empty — mast could not read one, and guessing would fabricate a label", d.Outcome)
		}
	})
}

// TestAppliedEditSurfaceIsUnchanged: the decision record is additive.
// AppliedEdit is a shipped surface — `mast sessions show` prints it,
// transcript.Detail projects it, the uat asserts on it — so the new
// record must ride alongside it under its own key, not replace it.
func TestAppliedEditSurfaceIsUnchanged(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: approve(map[string]any{
			"verdict":  "edit",
			"args":     map[string]any{"deployment": "api", "replicas": 2},
			"note":     "10 would exhaust the node pool",
			"approver": "user:sre-oncall",
		}),
	})
	edits := p.appliedEdits(t)
	if len(edits) != 1 {
		t.Fatalf("applied-edit records = %+v, want exactly one", edits)
	}
	e := edits[0]
	if e.Tool != "scale_deployment" || e.Approver != "user:sre-oncall" || e.Note != "10 would exhaust the node pool" {
		t.Errorf("AppliedEdit = %+v, want the W2.5 record unchanged", e)
	}
	if !strings.Contains(e.ProposedKey, "replicas=10") || !strings.Contains(e.ExecutedKey, "replicas=2") {
		t.Errorf("AppliedEdit keys = %q -> %q, want the W2.5 record unchanged", e.ProposedKey, e.ExecutedKey)
	}
	if e.Approver == RedactApprover(e.Approver) {
		t.Error("the applied-edit record must keep the raw approver: an operator reading `mast sessions show` needs the name, and redaction is an export-time decision")
	}
	if len(p.decisions(t)) != 1 {
		t.Error("the edit produced no decision record beside its applied-edit record")
	}
}

// TestRedactApprover pins the export default's two halves: a person
// becomes a stable digest, and a mechanism stays a mechanism.
func TestRedactApprover(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{"empty stays empty", "", ""},
		{
			// Stable across exports and across machines: the same
			// operator digests the same way a month later, which is what
			// makes "did the same person approve both?" answerable.
			"a person is digested",
			"user:sre-oncall",
			"sha256:" + digestOf(t, "user:sre-oncall"),
		},
		{
			// A proxied identity is one principal for this purpose;
			// splitting it would leak who proxies for whom.
			"a proxied person is digested whole",
			"alice@example.com (asserted by svc-gateway)",
			"sha256:" + digestOf(t, "alice@example.com (asserted by svc-gateway)"),
		},
		{
			// Digesting this would hide nothing (there is one of it) and
			// destroy the distinction the dataset needs most: whether a
			// human waved the change through or mast's own scheduler did.
			"an in-process mechanism passes through",
			"mast:internal",
			"mast:internal",
		},
		{"any mast: identity passes through", "mast:scheduler", "mast:scheduler"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RedactApprover(tt.in); got != tt.want {
				t.Errorf("RedactApprover(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	t.Run("distinct people get distinct digests", func(t *testing.T) {
		t.Parallel()
		if RedactApprover("user:a") == RedactApprover("user:b") {
			t.Error("two operators digested the same; the dataset could not tell them apart")
		}
	})
	t.Run("the digest is not reversible to the identity", func(t *testing.T) {
		t.Parallel()
		if got := RedactApprover("user:sre-oncall"); strings.Contains(got, "sre-oncall") {
			t.Errorf("RedactApprover leaked the identity: %q", got)
		}
	})
}

// digestOf recomputes the expected digest independently of the
// implementation's own helper, so the test would notice a change of
// algorithm rather than following it.
func digestOf(t *testing.T, s string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// TestDecisionRoundTrip: the record has to survive the session
// backend's state encoding, which hands strings back for some backends
// and decoded maps for others.
func TestDecisionRoundTrip(t *testing.T) {
	t.Parallel()
	want := Decision{
		DecidedAt:      time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
		Session:        "s-1",
		Workload:       "gke-triage",
		Specialist:     "remediator",
		FunctionCallID: "fc-1",
		Tool:           "scale_deployment",
		Outcome:        OutcomeEdit,
		Scope:          ScopeOnce,
		Authority:      AuthorityVerdict,
		Disposition:    DispositionAuthorized,
		ProposedKey:    "scale_deployment(replicas=10)",
		ProposedArgs:   map[string]any{"replicas": float64(10)},
		ExecutedKey:    "scale_deployment(replicas=2)",
		ExecutedArgs:   map[string]any{"replicas": float64(2)},
		Approver:       "user:sre-oncall",
		Note:           "too many",
	}
	raw, err := EncodeDecision(want)
	if err != nil {
		t.Fatalf("EncodeDecision: %v", err)
	}
	for _, form := range []any{raw, []byte(raw), decodedMap(t, raw)} {
		got, err := DecodeDecision(form)
		if err != nil {
			t.Fatalf("DecodeDecision(%T): %v", form, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("DecodeDecision(%T) = %+v, want %+v", form, got, want)
		}
	}
	if _, err := DecodeDecision("not json"); err == nil {
		t.Error("DecodeDecision accepted a non-record; a malformed row must be skippable, not silently empty")
	}
}

func decodedMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}
