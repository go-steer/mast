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
	"fmt"
	"sort"
	"strings"

	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/mast/pkg/permissions"
)

// Outcome is the operator's answer to a parked mutating call.
type Outcome string

const (
	// OutcomeApprove runs the call as the agent proposed it.
	OutcomeApprove Outcome = "approve"
	// OutcomeReject refuses the call. The agent is told not to retry and
	// not to look for another way to achieve the same effect.
	OutcomeReject Outcome = "reject"
	// OutcomeEdit runs the call with arguments the operator supplied in
	// place of the agent's. The operator's arguments are validated against
	// the tool's declared input schema, re-adjudicated against policy
	// under their own call key, and recorded durably as an AppliedEdit —
	// an edit that fails any of those is refused rather than narrowed
	// (edit.go).
	OutcomeEdit Outcome = "edit"
)

// Scope is how far an approval reaches. It is on the wire so that a
// client asking for more than one call gets an explicit refusal instead
// of a silent narrowing (docs/v0.3-plan.md W2.3): only ScopeOnce is
// admissible for a mutation, and the refusal is issued by
// permissions.Gate.RecordMutationVerdict, not here, so that the rule
// lives with the rest of the grant policy.
type Scope string

const (
	// ScopeOnce authorizes exactly this call. The default, and the only
	// scope a mutating call accepts.
	ScopeOnce Scope = "once"
	// ScopeSession asks to authorize this exact request for the session.
	ScopeSession Scope = "session"
	// ScopeSessionTool asks to authorize every call to this tool for the
	// session.
	ScopeSessionTool Scope = "session_tool"
	// ScopeAlways asks to persist a standing allowlist entry.
	ScopeAlways Scope = "always"

	// ScopeChangeSet authorizes this call and the other calls in the
	// change set it belongs to — the set the parked question showed
	// (W7). It is not a broader grant in the sense the scopes above
	// are: it names no tool and no pattern, only a finite list of exact
	// (tool, arguments) signatures the operator was shown, each of
	// which expires and is re-checked against the cluster before it
	// fires. Every one of them still passes the deny policy.
	//
	// mast refuses it rather than narrowing it when the set cannot be
	// granted — an unrecorded set, an edited verdict, an argument too
	// large to have been read. See writeGate.planGrants.
	ScopeChangeSet Scope = "change_set"
)

var scopeDecisions = map[Scope]permissions.Decision{
	ScopeOnce:        permissions.DecisionAllowOnce,
	ScopeSession:     permissions.DecisionAllowSession,
	ScopeSessionTool: permissions.DecisionAllowSessionTool,
	ScopeAlways:      permissions.DecisionAllowAlways,

	// Allow-once, deliberately: as far as the permissions gate is
	// concerned a change-set verdict authorizes the one call in hand,
	// which is the only call it is adjudicating. The set's other calls
	// are authorized by grants the write gate mints, and each of those
	// is adjudicated by this same gate when it fires.
	ScopeChangeSet: permissions.DecisionAllowOnce,
}

// Verdict is the operator's answer, carried in the Payload of the ADK
// tool confirmation that resumes a parked call
// (docs/orchestration-design.md, "Mutation approval").
//
// Approver is not client-supplied trust. Whatever a client puts there is
// overwritten at the resume boundary with the authenticated principal
// that presented the verdict (cmd/mast's verdictFor), so every verdict
// mast itself produces carries one. An OutcomeEdit that nonetheless
// reaches the gate without an approver is refused: an edit executes
// arguments no model proposed and no policy pattern vetted, so the
// attribution is the only record of where they came from.
type Verdict struct {
	Verdict  Outcome        `json:"verdict"`
	Scope    Scope          `json:"scope,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
	Note     string         `json:"note,omitempty"`
	Approver string         `json:"approver,omitempty"`
}

// Decision maps the verdict's scope onto the permissions decision the
// gate adjudicates. A reject is DecisionDeny regardless of scope — there
// is no such thing as denying more broadly than the call in hand.
func (v Verdict) Decision() (permissions.Decision, error) {
	if v.Verdict == OutcomeReject {
		return permissions.DecisionDeny, nil
	}
	scope := v.Scope
	if scope == "" {
		scope = ScopeOnce
	}
	d, ok := scopeDecisions[scope]
	if !ok {
		return permissions.DecisionDeny, fmt.Errorf("unknown approval scope %q (want one of %s)", v.Scope, knownScopes())
	}
	return d, nil
}

func knownScopes() string {
	names := make([]string, 0, len(scopeDecisions))
	for s := range scopeDecisions {
		names = append(names, string(s))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ConfirmationResponse builds the FunctionResponse payload that answers
// a parked mutating call. It is the exact wire shape ADK's
// RequestConfirmationRequestProcessor looks for before re-dispatching
// the original call: `confirmed` is the boolean it reads, `payload`
// carries the verdict it cannot express.
//
// It lives here, next to DecodeVerdict, so the writer and the reader of
// that shape stay in one place — the daemon's /resume handler and the
// eval rig both build it from this rather than each deriving it, which
// is what makes the rig's result evidence about mast.
//
// An edit is confirmed=true: the operator is authorizing a call, just
// not the one the model proposed. The gate decides which arguments run.
func ConfirmationResponse(v Verdict) map[string]any {
	return map[string]any{
		"confirmed": v.Verdict != OutcomeReject,
		"payload":   v,
	}
}

// DecodeVerdict reads mast's verdict record out of an ADK tool
// confirmation.
//
// ADK's own field is the boolean Confirmed, which cannot express an
// edit; mast's record rides in Payload. A client that speaks only ADK
// still works — a payload with no verdict field falls back to the
// boolean — and a client that speaks both must not contradict itself: a
// payload saying "approve" under Confirmed:false is a bug somewhere in
// the caller's serialization, and guessing which half meant it would be
// guessing about whether to mutate a production cluster.
//
// Everything unrecognized is an error rather than a default. The failure
// mode of a permissive decoder here is executing a mutation the operator
// did not authorize.
func DecodeVerdict(c *toolconfirmation.ToolConfirmation) (Verdict, error) {
	if c == nil {
		return Verdict{}, fmt.Errorf("approval: no tool confirmation to decode")
	}
	v := Verdict{}
	if c.Payload != nil {
		// Payload is `any`: a typed value in-process, a map[string]any
		// once it has been through the event log's JSON round trip. One
		// re-marshal handles both.
		raw, err := json.Marshal(c.Payload)
		if err != nil {
			return Verdict{}, fmt.Errorf("approval: re-marshalling confirmation payload: %w", err)
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return Verdict{}, fmt.Errorf("approval: confirmation payload is not a verdict record: %w", err)
		}
	}
	if v.Verdict == "" {
		if c.Confirmed {
			v.Verdict = OutcomeApprove
		} else {
			v.Verdict = OutcomeReject
		}
		return v, nil
	}
	switch v.Verdict {
	case OutcomeApprove, OutcomeEdit:
		if !c.Confirmed {
			return Verdict{}, fmt.Errorf("approval: contradictory verdict: %q with confirmed=false", v.Verdict)
		}
	case OutcomeReject:
		if c.Confirmed {
			return Verdict{}, fmt.Errorf("approval: contradictory verdict: %q with confirmed=true", v.Verdict)
		}
	default:
		return Verdict{}, fmt.Errorf("approval: unknown verdict %q (want approve, reject, or edit)", v.Verdict)
	}
	return v, nil
}
