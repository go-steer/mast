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

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// EditStateKeyPrefix namespaces the durable record of an applied edit in
// the session's state, one key per function call.
//
// The record exists because the event log alone cannot answer "what
// executed?" after an edit. ADK re-fires the parked call verbatim, so
// the durable FunctionCall part still carries the arguments the *model*
// proposed, and the FunctionResponse next to it is the result of running
// the *operator's*. Reading the pair without this record gives an
// operator a confident, wrong answer about what mast did to their
// cluster (docs/v0.3-plan.md W2.5, "an audit gap the workstream has to
// close, not inherit").
const EditStateKeyPrefix = "mast_approval_edit_"

// EditStateKey is the state key holding the AppliedEdit record for one
// function call.
func EditStateKey(functionCallID string) string {
	return EditStateKeyPrefix + functionCallID
}

// AppliedEdit is the durable record of an operator's edit being applied:
// what the model asked for, what actually ran, and who authorized the
// substitution. Stored as a JSON string so it survives every session
// backend's state encoding unchanged.
type AppliedEdit struct {
	Tool         string         `json:"tool"`
	Approver     string         `json:"approver"`
	ProposedKey  string         `json:"proposed_key"`
	ExecutedKey  string         `json:"executed_key"`
	ProposedArgs map[string]any `json:"proposed_args"`
	ExecutedArgs map[string]any `json:"executed_args"`
	Note         string         `json:"note,omitempty"`
}

// String renders the record for an operator-facing listing.
func (e AppliedEdit) String() string {
	s := e.ExecutedKey + " (proposed " + e.ProposedKey + ")"
	if e.Approver != "" {
		s += " approved by " + e.Approver
	}
	if e.Note != "" {
		s += ": " + e.Note
	}
	return s
}

// DecodeAppliedEdit parses a record written under an EditStateKey. The
// value is whatever the session backend handed back: the JSON string
// that was written, or — for a backend that decodes state values — the
// map it decodes to.
func DecodeAppliedEdit(v any) (AppliedEdit, error) {
	var raw []byte
	switch t := v.(type) {
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return AppliedEdit{}, fmt.Errorf("approval: re-marshalling applied-edit record: %w", err)
		}
		raw = b
	}
	var e AppliedEdit
	if err := json.Unmarshal(raw, &e); err != nil {
		return AppliedEdit{}, fmt.Errorf("approval: applied-edit record is not one: %w", err)
	}
	return e, nil
}

// declarer is the ADK-side interface that exposes a tool's input schema.
// Both function tools and MCP tools implement it; tool.Tool itself does
// not, which is why this is an assertion rather than a parameter.
type declarer interface {
	Declaration() *genai.FunctionDeclaration
}

// normalizeEdit validates an operator's arguments against the tool's
// declared input schema and returns them in the shape the tool will
// receive.
//
// Three refusals, in order, and all of them are the legibility rule the
// workstream copied from LangChain's agent: an operator can only edit a
// call mast can describe and check. A tool that declares no schema, an
// argument the tool does not declare, and a value the schema rejects are
// all cases where mast would be executing an argument set nobody — not
// the model, not the schema, not a policy pattern — has vetted.
func normalizeEdit(t tool.Tool, edited map[string]any) (map[string]any, error) {
	if len(edited) == 0 {
		return nil, fmt.Errorf("the verdict is %q but carries no arguments", OutcomeEdit)
	}
	schema, err := inputSchema(t)
	if err != nil {
		return nil, err
	}
	// One JSON round trip puts the operator's values in the same shape a
	// model's would arrive in — numbers as float64, structs as maps — so
	// that what the schema validates is exactly what the tool receives.
	raw, err := json.Marshal(edited)
	if err != nil {
		return nil, fmt.Errorf("edited arguments are not JSON: %w", err)
	}
	var norm map[string]any
	if err := json.Unmarshal(raw, &norm); err != nil {
		return nil, fmt.Errorf("edited arguments are not a JSON object: %w", err)
	}
	if err := checkDeclaredKeys(schema, norm); err != nil {
		return nil, err
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("tool %q input schema does not resolve: %w", t.Name(), err)
	}
	if err := resolved.Validate(norm); err != nil {
		return nil, fmt.Errorf("edited arguments do not satisfy tool %q's input schema: %w", t.Name(), err)
	}
	return norm, nil
}

// inputSchema reads a tool's declared parameters as a JSON Schema.
//
// ADK carries the schema in two mutually exclusive fields: MCP tools and
// function tools set ParametersJsonSchema (a *jsonschema.Schema), while
// a declaration built by hand may use the genai.Schema in Parameters.
// Both are re-marshalled rather than type-asserted, because the field is
// typed `any` and its dynamic type is the provider's business.
func inputSchema(t tool.Tool) (*jsonschema.Schema, error) {
	d, ok := t.(declarer)
	if !ok {
		return nil, fmt.Errorf("tool %q does not declare its arguments, so an edit cannot be checked against anything", t.Name())
	}
	decl := d.Declaration()
	if decl == nil {
		return nil, fmt.Errorf("tool %q has no declaration, so an edit cannot be checked against anything", t.Name())
	}
	var src any
	switch {
	case decl.ParametersJsonSchema != nil:
		src = decl.ParametersJsonSchema
	case decl.Parameters != nil:
		src = decl.Parameters
	default:
		return nil, fmt.Errorf("tool %q declares no input schema, so an edit cannot be checked against anything", t.Name())
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("tool %q input schema is not marshallable: %w", t.Name(), err)
	}
	schema := &jsonschema.Schema{}
	if err := json.Unmarshal(raw, schema); err != nil {
		return nil, fmt.Errorf("tool %q input schema is not a JSON Schema: %w", t.Name(), err)
	}
	return schema, nil
}

// checkDeclaredKeys refuses an argument the tool does not declare.
//
// JSON Schema only rejects those on its own when the author wrote
// additionalProperties:false, and an MCP server's schema usually did
// not. The rule mast wants is narrower than the schema's: the operator
// is editing a named call, so every key they send has to be one the tool
// named. A schema with no declared properties is treated as free-form
// and left to the validator.
func checkDeclaredKeys(schema *jsonschema.Schema, args map[string]any) error {
	if len(schema.Properties) == 0 {
		return nil
	}
	var unknown []string
	for k := range args {
		if _, ok := schema.Properties[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	declared := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		declared = append(declared, k)
	}
	sort.Strings(declared)
	return fmt.Errorf("edited arguments name %s, which the tool does not declare (it declares %s)",
		strings.Join(unknown, ", "), strings.Join(declared, ", "))
}

// applyArgs replaces the live argument map's contents in place.
//
// In place is the whole mechanism: ADK's flow passes this same map to
// the tool after the before-tool callback returns, so a replacement map
// would be discarded and the model's arguments would execute
// (adkseam_test.go, TestSeamEditedArgumentsAreWhatExecutes).
func applyArgs(args, edited map[string]any) {
	for k := range args {
		delete(args, k)
	}
	for k, v := range edited {
		args[k] = v
	}
}
