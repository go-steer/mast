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
	"fmt"
	"strings"
	"time"
)

// DecisionStateKeyPrefix namespaces the durable record of one
// adjudication in the session's state, one key per function call.
//
// It exists because the three answers an operator can give leave three
// different traces, and none of them is the whole thing (v0.4 W8):
//
//   - approve leaves a FunctionCall and a FunctionResponse that look
//     exactly like an ungated call;
//   - reject leaves a FunctionResponse carrying a refusal string, which
//     is indistinguishable from a tool that happened to fail;
//   - edit leaves an AppliedEdit, which is the only one of the three
//     that is already a record — and only because W2.5 needed it to
//     answer "what ran?".
//
// A fleet's adjudications are worth more than that. Read together they
// are the closest thing an operator has to labelled data about their own
// judgement — which calls a human waves through, which they refuse, and
// which they correct and how. That is only harvestable if all three land
// in the same shape, so the write gate records one Decision per
// adjudication whatever the answer was, and pkg/transcript exports them.
//
// AppliedEdit is deliberately not replaced by it. It is a shipped
// surface — `mast sessions show` prints it, transcript.Detail projects
// it, the uat asserts on it — and folding it into this record would
// break all three to save a duplicated field pair.
const DecisionStateKeyPrefix = "mast_decision_"

// DecisionStateKey is the state key holding the Decision record for one
// function call.
func DecisionStateKey(functionCallID string) string {
	return DecisionStateKeyPrefix + functionCallID
}

// DecisionSchema names the record shape in an export's provenance
// header, so a consumer reading a file written by an older mast can
// tell what it is holding rather than inferring it from the fields that
// happen to be present.
const DecisionSchema = "mast.decision/v1"

// Disposition is what the gate did with the call, as distinct from what
// the operator asked for. The two come apart more often than they look
// like they should: an approved call can still be refused by the
// permissions policy, and an edit can be refused for arguments the tool
// does not declare. A dataset that recorded only the operator's answer
// would be labelling the wrong variable.
type Disposition string

const (
	// DispositionAuthorized means the gate let the call through. It says
	// nothing about whether the tool then succeeded — that is the
	// FunctionResponse's business, not the gate's.
	DispositionAuthorized Disposition = "authorized"

	// DispositionRefusedByOperator means a person said no.
	DispositionRefusedByOperator Disposition = "refused_by_operator"

	// DispositionRefusedByMast means mast refused a verdict a person
	// gave: a malformed payload, an edit it could not attribute or
	// validate, a scope it does not grant, a configured deny the
	// operator's edit walked into. Refusal carries which.
	DispositionRefusedByMast Disposition = "refused_by_mast"
)

// Authority is where the authorization for this call came from.
type Authority string

const (
	// AuthorityVerdict is the ordinary case: a person was asked about
	// this exact call and answered it.
	AuthorityVerdict Authority = "operator_verdict"

	// AuthorityChangeSetGrant is a call that fired on an answer given
	// earlier about a different call in the same change set (W7).
	//
	// These are recorded too, and the reason is the whole point of the
	// workstream: an export that held only the calls a human was asked
	// about would show one approved scale_deployment where four ran, and
	// would be a quietly false description of what the operator
	// authorized.
	AuthorityChangeSetGrant Authority = "change_set_grant"
)

// Decision is the durable record of one adjudication of one mutating
// call: what was proposed, what the operator answered, what actually
// ran, and who decided.
//
// Stored as a JSON string in the session state, like AppliedEdit and
// Grant, so it survives every session backend's state encoding
// unchanged.
//
// Approver is stored raw. Redaction is an export-time decision
// (RedactApprover) rather than a storage-time one, because the operator
// surface an on-call engineer reads — `mast sessions show`, the daemon's
// own audit log — must be able to name the person who approved a change
// to their cluster. What must not leak is the *export*, which travels.
type Decision struct {
	// DecidedAt is when the gate adjudicated, not when the operator
	// answered — mast does not see the latter.
	DecidedAt time.Time `json:"decided_at"`

	// Session, Workload, Specialist and Invocation are what makes a row
	// legible without the session it came from. A dataset of calls with
	// no context is a dataset of trivia: "someone rejected
	// scale_deployment(api, 10)" is only a label if you can also say
	// which workload was running and which specialist proposed it.
	Session    string `json:"session"`
	Workload   string `json:"workload,omitempty"`
	Specialist string `json:"specialist,omitempty"`
	Invocation string `json:"invocation,omitempty"`

	// FunctionCallID keys the record and is the join back to the
	// transcript's FunctionCall/FunctionResponse pair.
	FunctionCallID string `json:"function_call_id,omitempty"`

	Tool string `json:"tool"`

	// Outcome is the operator's answer. Empty when there was not a
	// readable one — a malformed verdict is recorded rather than
	// dropped, because "clients keep sending mast payloads it cannot
	// read" is itself a finding.
	Outcome Outcome `json:"outcome,omitempty"`
	Scope   Scope   `json:"scope,omitempty"`

	Authority   Authority   `json:"authority"`
	Disposition Disposition `json:"disposition"`

	// Refusal is the machine-readable code the model was told, when the
	// call did not run: denied_by_operator, edit_refused,
	// denied_by_policy, and so on.
	Refusal string `json:"refusal,omitempty"`

	// ChangeSet names the approved set a granted call fired under
	// (Grant.Origin). Empty for an ordinary verdict.
	ChangeSet string `json:"change_set,omitempty"`

	// ProposedKey/ProposedArgs are the call as the model asked for it.
	ProposedKey  string         `json:"proposed_key"`
	ProposedArgs map[string]any `json:"proposed_args,omitempty"`

	// ExecutedKey/ExecutedArgs are set only when they differ from the
	// proposal — that is, on an edit. The proposed→executed pair is the
	// densest signal in the whole record: it is a human writing down
	// what the model should have said.
	ExecutedKey  string         `json:"executed_key,omitempty"`
	ExecutedArgs map[string]any `json:"executed_args,omitempty"`

	Approver string `json:"approver,omitempty"`
	Note     string `json:"note,omitempty"`
}

// Edited reports whether the operator substituted their own arguments.
func (d Decision) Edited() bool { return d.ExecutedKey != "" && d.ExecutedKey != d.ProposedKey }

// EncodeDecision renders the record for storage.
func EncodeDecision(d Decision) (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("approval: encoding decision record: %w", err)
	}
	return string(raw), nil
}

// DecodeDecision parses a record written under a DecisionStateKey. The
// value is whatever the session backend handed back: the JSON string
// that was written, or — for a backend that decodes state values — the
// map it decodes to.
func DecodeDecision(v any) (Decision, error) {
	var raw []byte
	switch t := v.(type) {
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return Decision{}, fmt.Errorf("approval: re-marshalling decision record: %w", err)
		}
		raw = b
	}
	var d Decision
	if err := json.Unmarshal(raw, &d); err != nil {
		return Decision{}, fmt.Errorf("approval: decision record is not one: %w", err)
	}
	return d, nil
}

// MachineApproverPrefix marks an identity that names a mechanism rather
// than a person: mast:internal for an in-process caller (the timed-pause
// scheduler, boot-time auto-resume), mast:scheduler, and any future
// sibling. cmd/mast mints these itself; no authenticated caller can
// present one, because the daemon overwrites the payload's approver with
// the authenticated principal at the resume boundary.
const MachineApproverPrefix = "mast:"

// RedactedApproverPrefix marks a digested identity in an export, so a
// consumer can never mistake one for a login name.
const RedactedApproverPrefix = "sha256:"

// RedactApprover replaces an approver identity with a stable digest of
// it.
//
// The point of the digest, rather than dropping the field: a decision
// dataset needs to be able to answer "did the same person approve both
// of these?" — inter-approver consistency is most of what makes fleet
// adjudications interesting — and that question does not need anybody's
// name. Two exports taken a month apart digest the same person the same
// way, so the answer survives across files.
//
// Machine identities pass through in the clear. Digesting mast:internal
// would hide nothing (there is exactly one of it, and the digest is a
// constant anyone can compute) while destroying the one distinction a
// consumer actually needs: whether a change was waved through by a human
// or by mast's own scheduler. A dataset that cannot separate those is
// not a dataset about human judgement.
//
// The whole string is digested, including cmd/mast's
// "alice@corp (asserted by svc-proxy)" spelling. Splitting it to keep
// the proxy in the clear would leak the shape of who proxies for whom,
// and the pair is one principal for the purpose of "same approver?".
//
// The truncation length follows grant.go's digestResult: 16 hex
// characters, 64 bits, which is far past collision risk for the number
// of distinct operators any fleet has.
func RedactApprover(approver string) string {
	if approver == "" {
		return ""
	}
	if strings.HasPrefix(approver, MachineApproverPrefix) {
		return approver
	}
	sum := sha256.Sum256([]byte(approver))
	return RedactedApproverPrefix + hex.EncodeToString(sum[:])[:16]
}

// Redacted returns a copy of the record with the approver digested.
// Argument values are untouched, deliberately — see the note on
// transcript.ExportOptions.
func (d Decision) Redacted() Decision {
	d.Approver = RedactApprover(d.Approver)
	return d
}
