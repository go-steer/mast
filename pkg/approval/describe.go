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

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"
)

// Parked describes a mutating call waiting on an operator, read back
// out of the durable session log.
//
// The gate writes the question; this reads it. The two are in the same
// package because the args of an adk_request_confirmation call are an
// ADK-internal shape that mast happens to be able to read, not a
// contract — a reader that lived somewhere else would drift.
type Parked struct {
	// Hint is the one-line question the gate wrote.
	Hint string
	// CallID is the function-call ID of the call this question gates —
	// the ID the approved call re-fires under, and the ID every other
	// durable record of the same call is keyed by (DecisionStateKey,
	// EditStateKey, CaptureStateKey). It is therefore the join: a reader
	// holding a park and a decision has no other way to know they are
	// about the same call.
	//
	// pkg/effects reads the same field for its own purpose and does not
	// route through this type, because pkg/effects does not import this
	// package and widening it for one accessor is the wrong trade. Both
	// readers are pinned against a real flow by adkseam_test.go.
	CallID string
	// Tool is the parked call's tool name, "" if it could not be read.
	Tool string
	// Args are the arguments the agent proposed.
	Args map[string]any
	// Request is the gate's own payload (an approval.Request), as the
	// map the event log round-trips it to. Nil when the confirmation
	// came from somewhere other than mast's write gate — a tool that
	// calls RequestConfirmation itself, for instance.
	Request map[string]any
}

// DescribeConfirmation reads a parked mutating call out of the Args of
// an adk_request_confirmation function call
// (internal/llminternal/functions.go builds them:
// {"originalFunctionCall": *genai.FunctionCall, "toolConfirmation":
// toolconfirmation.ToolConfirmation}).
//
// Both value shapes are handled. In-process — an attach client tailing
// live events — the args hold the typed structs; read back from the
// event log they are the maps JSON leaves behind. Anything unreadable
// is left zero rather than guessed at: this feeds an operator deciding
// whether to change a cluster, and a plausible-looking reconstruction
// is worse than a blank.
func DescribeConfirmation(args map[string]any) Parked {
	var p Parked
	switch fc := args["originalFunctionCall"].(type) {
	case *genai.FunctionCall:
		if fc != nil {
			p.CallID, p.Tool, p.Args = fc.ID, fc.Name, fc.Args
		}
	case genai.FunctionCall:
		p.CallID, p.Tool, p.Args = fc.ID, fc.Name, fc.Args
	case map[string]any:
		p.CallID, _ = fc["id"].(string)
		p.Tool, _ = fc["name"].(string)
		p.Args, _ = fc["args"].(map[string]any)
	}
	if tc, ok := args["toolConfirmation"]; ok {
		var decoded struct {
			Hint    string         `json:"hint"`
			Payload map[string]any `json:"payload"`
		}
		if raw, err := json.Marshal(tc); err == nil {
			if err := json.Unmarshal(raw, &decoded); err == nil {
				p.Hint = decoded.Hint
				p.Request = decoded.Payload
			}
		}
	}
	return p
}

// Summary is the operator-facing one-liner for a parked call: the hint
// when the gate wrote one, the rendered call otherwise, and an honest
// admission when neither could be read.
func (p Parked) Summary() string {
	switch {
	case p.Hint != "":
		return p.Hint
	case p.Tool != "":
		return fmt.Sprintf("Approve mutating call %s?", CallKey(p.Tool, p.Args))
	}
	return "Approve a mutating tool call? (the parked call could not be read from the event log)"
}

// VerdictSchema is the JSON schema a resume payload answering a parked
// mutating call must satisfy. It has been three-valued since W2.1 —
// before `edit` was executable — because the schema is written into the
// durable log at pause time, and a session paused under a two-valued
// schema and resumed after an upgrade is a migration across exactly the
// restart boundary the write gate exists to survive.
//
// Approver is deliberately absent: it is not the client's to state. The
// resume boundary overwrites it with the authenticated caller.
func VerdictSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "object",
		Description: "Operator verdict on a parked mutating tool call.",
		Properties: map[string]*jsonschema.Schema{
			"verdict": {
				Type:        "string",
				Enum:        []any{string(OutcomeApprove), string(OutcomeReject), string(OutcomeEdit)},
				Description: "approve runs the call as proposed; reject refuses it; edit runs it with the arguments in `args`, which must validate against the tool's own input schema and are re-checked against policy before they run.",
			},
			"scope": {
				Type:        "string",
				Enum:        []any{string(ScopeOnce), string(ScopeChangeSet)},
				Description: "How far the approval reaches. `once` authorizes this call. `change_set` additionally authorizes the other calls listed under the request's `change_set`, each bound to its exact arguments, expiring, and re-checked against the cluster before it fires; it is admissible only when the request carries one. Anything else is refused, not narrowed.",
			},
			"args": {
				Type:        "object",
				Description: "edit only: the arguments to run instead of the agent's.",
			},
			"note": {
				Type:        "string",
				Description: "Optional free text; shown to the agent.",
			},
		},
		Required: []string{"verdict"},
	}
}
