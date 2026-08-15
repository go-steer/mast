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
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
)

// The change set: what a diagnosis specialist proposes, in the only
// form an operator can be asked to approve without guessing.
//
// A `recommended_actions` list is prose addressed to a human — "raise
// the memory limit on the api Deployment" — and prose cannot be
// approved, only agreed with. What actually reaches the cluster is a
// tool call the executor composes later, from the same prose, on its
// own turn. The operator approved a sentence; the cluster received a
// call nobody looked at. That gap is the whole reason this file exists
// (docs/v0.3-plan.md W7.0, and W2 finding (c), the live-cluster run
// that found it).
//
// A change set closes it by making the finding carry the call: a
// possibly-empty list of (tool, arguments) drawn from the workload's
// own tool_catalog, each checked against the named tool's declared
// input schema at the moment the finding is returned. Empty is a
// first-class answer — "raise the memory limit, but I don't know to
// what" is an honest diagnosis, and the specialist says why in
// `escalate` rather than inventing a number to fill a field.

// ChangeSetField is the report property a change set travels in. It is
// a property of the *workload's* report schema (gke-triage's
// schemas/finding.json declares it), not something mast injects — mast
// only recognizes the name.
const ChangeSetField = "proposed_change"

// ChangeSetStateKeyPrefix namespaces the durable record of the change
// set one specialist proposed, one key per specialist.
const ChangeSetStateKeyPrefix = "mast_change_set_"

// ChangeSetStateKey is the state key holding a specialist's proposed
// change set.
//
// Durable, because the predicate that routes a finding to the change
// executor has to survive the approval pause. Under graph dispatch a
// confirmation resume re-enters at START and upstream nodes genuinely
// re-execute (docs/spike-findings.md, W2.1's asymmetry), so a decision
// held in a Go variable from the first pass is not there on the second.
func ChangeSetStateKey(specialist string) string {
	return ChangeSetStateKeyPrefix + specialist
}

// ProposedChange is one machine-executable remediation: a tool the
// workload declares, and the arguments to call it with.
type ProposedChange struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// UnmarshalJSON accepts `arguments` as either a JSON object or a JSON
// string holding one.
//
// The string spelling is not a convenience, it is forced. A model's
// report is validated by ADK against the declared output schema, and
// that validation walks nested objects refusing any key the schema did
// not declare (adk/v2 internal/utils.ValidateMapOnSchema via matchType
// — see TestADKRefusesUndeclaredKeysInNestedObjects). An arguments
// object is free-form by definition: its keys are whichever tool the
// finding names. There is no schema that admits it, so the wire form is
// a string and mast parses it and checks it against the real tool's
// real schema — which is a stronger check than the report schema could
// have made anyway. mast's own roster loader agrees from the other
// direction: pkg/specialists.checkSchema refuses an object property
// with no declared properties, because it would accept anything.
//
// The object spelling is accepted because that is what mast writes:
// durable records, operator payloads and grants all carry the parsed
// arguments, and a round trip through those must not have to re-encode.
func (c *ProposedChange) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return fmt.Errorf("approval: %s entry is not an object: %w", ChangeSetField, err)
	}
	c.Tool = wire.Tool
	c.Arguments = nil
	if len(wire.Arguments) == 0 || string(wire.Arguments) == "null" {
		return nil
	}
	body := wire.Arguments
	var asString string
	if err := json.Unmarshal(body, &asString); err == nil {
		trimmed := strings.TrimSpace(asString)
		if trimmed == "" {
			return nil
		}
		body = []byte(trimmed)
	}
	if err := json.Unmarshal(body, &c.Arguments); err != nil {
		return fmt.Errorf("approval: arguments for tool %q are not a JSON object: %w", c.Tool, err)
	}
	return nil
}

// Signature renders a change as a byte-stable identity for the exact
// call it proposes.
//
// This is what "the operator approves the object that fires" reduces
// to: the call parked at the write gate has to render to the same bytes
// as the change the operator approved, or the claim is about intent
// rather than about the call. CallKey cannot serve — it elides values
// over 120 characters for legibility, so two different manifests share
// a key — and neither can Go's map iteration order, which is why this
// goes through encoding/json (which sorts object keys, at every depth).
//
// Arguments must be JSON-encodable. NormalizeArgs guarantees that by
// construction; the error return is what stops a caller who skipped it
// from getting a signature that quietly compares equal to something
// else.
func Signature(toolName string, args map[string]any) (string, error) {
	if len(args) == 0 {
		return toolName + "({})", nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("approval: arguments for tool %q cannot be encoded, so the call has no stable signature: %w", toolName, err)
	}
	return toolName + "(" + string(raw) + ")", nil
}

// Signature is the change's call signature. See Signature.
func (c ProposedChange) Signature() (string, error) { return Signature(c.Tool, c.Arguments) }

// ParseChangeSet reads the change set out of a specialist's structured
// report.
//
// A report with no ChangeSetField carries no change set — that is the
// common case and not an error, since only a roster whose report schema
// declares the field can ever produce one. A field that is present but
// is not a list of changes IS an error: the specialist tried to say
// something about remediation and mast could not read it, and passing
// that on as "no change proposed" would silently drop a proposal.
func ParseChangeSet(report map[string]any) ([]ProposedChange, error) {
	v, ok := report[ChangeSetField]
	if !ok || v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("approval: %s is not encodable: %w", ChangeSetField, err)
	}
	var changes []ProposedChange
	if err := json.Unmarshal(raw, &changes); err != nil {
		return nil, fmt.Errorf("approval: %s is not a list of {tool, arguments} entries: %w", ChangeSetField, err)
	}
	return changes, nil
}

// ChangeSetChecker validates a proposed change set against the
// workload's catalog and the named tools' declared input schemas.
//
// Both fields are required, and they answer different questions.
// Declares is "may this workload call this tool at all" — the catalog
// is the workload's whole reachable surface (W2.2/W2.4), so a change
// naming something outside it is a change nothing could execute.
// Schema is "are these the arguments that tool takes" — which needs the
// live tool, because the catalog carries names and a mutating flag and
// deliberately no schemas.
type ChangeSetChecker struct {
	// Declares reports whether the workload's tool_catalog names this
	// tool.
	Declares func(toolName string) bool

	// Schema resolves the tool's declared input schema by name. It is
	// consulted only for tools Declares accepted.
	Schema func(toolName string) (*jsonschema.Schema, error)
}

// Check validates every entry and returns the change set with each
// entry's arguments normalized into the shape the tool will receive.
//
// The first failure wins and names the entry. A specialist that gets
// this back has to fix one thing and re-report; handing it a list of
// every problem at once invites it to rewrite the whole finding.
func (c ChangeSetChecker) Check(changes []ProposedChange) ([]ProposedChange, error) {
	if c.Declares == nil || c.Schema == nil {
		return nil, fmt.Errorf("approval: ChangeSetChecker needs both Declares and Schema")
	}
	out := make([]ProposedChange, 0, len(changes))
	for i, ch := range changes {
		name := strings.TrimSpace(ch.Tool)
		if name == "" {
			return nil, fmt.Errorf("%s[%d] names no tool", ChangeSetField, i)
		}
		if !c.Declares(name) {
			return nil, fmt.Errorf("%s[%d] names tool %q, which this workload's tool_catalog does not declare; propose a call this workload can make, or return an empty %s and say why in escalate",
				ChangeSetField, i, name, ChangeSetField)
		}
		schema, err := c.Schema(name)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", ChangeSetField, i, err)
		}
		norm, err := NormalizeArgs(name, schema, ch.Arguments)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", ChangeSetField, i, err)
		}
		out = append(out, ProposedChange{Tool: name, Arguments: norm})
	}
	return out, nil
}

// FinishTaskToolName is the completion tool ADK auto-installs on every
// Task-mode agent, and the one place a specialist's structured report
// exists as arguments mast can inspect before it becomes a result.
//
// Hard-coded rather than imported: ADK keeps the constant in
// internal/workflowinternal. TestFinishTaskIsTheReportSeam pins the
// name against a live Task agent, so a rename upstream fails a test
// here instead of quietly disabling the producer contract.
const FinishTaskToolName = "finish_task"

// checkChangeSet enforces the producer contract on a specialist's
// report. A non-nil return is the refusal, which becomes finish_task's
// function response.
//
// Refusing this way — rather than erroring the turn — is what "rejected
// back to the specialist" means, and it is ADK's own convention:
// finish_task's validation failures come back as an error response, and
// llmagent treats any non-success response as "keep going", so the
// model sees what was wrong and gets another turn to fix it. A
// specialist that cannot name an executable change has an out that
// always works: an empty list, and the reason in `escalate`.
func (g *writeGate) checkChangeSet(ctx agent.Context, t tool.Tool, args map[string]any) map[string]any {
	if g.cfg.ChangeSet == nil || t.Name() != FinishTaskToolName {
		return nil
	}
	changes, err := ParseChangeSet(args)
	if err == nil && len(changes) > 0 {
		changes, err = g.cfg.ChangeSet.Check(changes)
	}
	if err != nil {
		g.audit(ctx, t, FinishTaskToolName, "change_set_refused", err.Error())
		return map[string]any{
			"error": "invalid_proposed_change",
			"detail": "Your report was NOT accepted: " + err.Error() + ". " +
				"Fix that one entry and call finish_task again. `" + ChangeSetField + "` must name a tool this workload declares and carry that tool's own arguments, " +
				"encoded as a JSON object in a string. If you cannot name an exact executable call, send an empty list and say what is missing in `escalate` — " +
				"that is a valid report, and inventing a plausible-looking call is not.",
		}
	}
	if len(changes) == 0 {
		return nil
	}
	g.recordChangeSet(ctx, changes)
	return nil
}

// recordChangeSet writes the validated change set into the session's
// durable state, keyed by the specialist that proposed it.
//
// It rides the state delta of the event ADK is already appending for
// this call — same session, same write, no second writer competing for
// the runner's write lease (mast #45/#46), the same discipline
// recordEdit follows.
//
// One writer, one key. The routing decision downstream ("did this
// finding propose something the executor should carry out?") reads this
// record rather than re-deriving it from the report, because on a
// confirmation resume the graph re-enters at START and the first pass's
// conclusions are gone (docs/spike-findings.md). Re-deriving in two
// places is how the approved object and the executed object drift
// apart, which is the failure this whole workstream exists to prevent.
func (g *writeGate) recordChangeSet(ctx agent.Context, changes []ProposedChange) {
	raw, err := EncodeChangeSet(changes)
	if err != nil {
		g.cfg.Logger.Error("write gate: a specialist proposed a change set that could not be recorded",
			"specialist", ctx.AgentName(), "error", err.Error())
		return
	}
	if a := ctx.Actions(); a != nil {
		if a.StateDelta == nil {
			a.StateDelta = map[string]any{}
		}
		a.StateDelta[ChangeSetStateKey(ctx.AgentName())] = raw
	}
	sigs := make([]string, 0, len(changes))
	for _, ch := range changes {
		if sig, err := ch.Signature(); err == nil {
			sigs = append(sigs, sig)
		}
	}
	g.cfg.Logger.Info("change set proposed",
		"specialist", ctx.AgentName(), "changes", len(changes),
		"calls", strings.Join(sigs, "; "),
		"session", ctx.SessionID(), "invocation", ctx.InvocationID())
}

// DescribeChangeSet renders a change set for a prompt or an operator
// message: one canonical signature per line, in list order.
func DescribeChangeSet(changes []ProposedChange) string {
	lines := make([]string, 0, len(changes))
	for i, ch := range changes {
		sig, err := ch.Signature()
		if err != nil {
			sig = ch.Tool + "(<unencodable arguments>)"
		}
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, sig))
	}
	return strings.Join(lines, "\n")
}

// DecodeChangeSet reads a change set back out of durable state. The
// value is whatever the session backend handed back: the JSON string
// mast wrote, or the decoded shape a backend that decodes state values
// returns.
func DecodeChangeSet(v any) ([]ProposedChange, error) {
	if v == nil {
		return nil, nil
	}
	var raw []byte
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("approval: re-marshalling change-set record: %w", err)
		}
		raw = b
	}
	var changes []ProposedChange
	if err := json.Unmarshal(raw, &changes); err != nil {
		return nil, fmt.Errorf("approval: change-set record is not one: %w", err)
	}
	return changes, nil
}

// EncodeChangeSet renders a change set for durable state. Stored as a
// JSON string so it survives every session backend's state encoding
// unchanged — the same reason AppliedEdit is.
func EncodeChangeSet(changes []ProposedChange) (string, error) {
	raw, err := json.Marshal(changes)
	if err != nil {
		return "", fmt.Errorf("approval: encoding change-set record: %w", err)
	}
	return string(raw), nil
}
