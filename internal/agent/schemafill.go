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

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
)

// Schema-conforming finish_task arguments for the offline fake models.
//
// A Task-mode specialist that declares `output_schema:` (W1.3) gets that
// schema as its finish_task parameters verbatim, and ADK validates the
// call against it — a violation comes back to the model as an error
// function response rather than becoming the agent's result. Both fakes
// used to answer with a hard-coded `{"result": "<some text>"}`, which is
// exactly such a violation: against the shipped gke-triage roster the
// fake looped, got refused, looped again, and the run died on the
// specialist's turn cap with no report. Offline runs of the anchor
// workload were broken from the moment W1.3 landed and nothing said so,
// because no test drove the shipped bundle end to end. `U-report`
// (scripts/uat-v0.3.sh) is that test and this file is what makes it
// possible.
//
// The rule is: answer the contract you were handed. The fakes read the
// finish_task declaration off the request and synthesize a value for
// every property in it, so a roster can change its report shape without
// anyone touching a fake. Where no output schema is declared, ADK's
// default single-`result` wrapper is in force and the caller's own text
// is used, byte for byte as before.

// fakeSchemaViolationEnv, when set to a non-empty value, makes the
// offline fakes deliberately emit a finish_task call that VIOLATES a
// declared output schema (the first required property is omitted), then
// give up with plain text once the runtime refuses it.
//
// This exists so a black-box harness can show that the refusal is real:
// a conforming report and a violating one have to reach visibly
// different ends, or "the report conforms" is a claim about the fake
// rather than about mast. Nothing in production reads this variable —
// it is only consulted through the fakes, which are themselves
// test-only models.
const fakeSchemaViolationEnv = "MAST_FAKE_SCHEMA_VIOLATION"

func violateSchemaEnabled() bool { return os.Getenv(fakeSchemaViolationEnv) != "" }

// fakeProposedChangeEnv seeds the change-set field (W7.0) of a
// synthesized report.
//
// The default is an empty list, and that default is load-bearing rather
// than lazy: the change set is the one report field whose values are
// checked against the world outside the schema — each entry has to name
// a tool the workload declares and carry arguments that fit that tool's
// input schema. A synthesized entry names a synthesized tool, which the
// producer contract correctly refuses, which would leave the fake
// retrying a report it cannot fix until the specialist's turn cap
// killed the run. An empty list is the honest answer for a model that
// diagnosed nothing: no call to propose.
//
// A harness that wants a non-empty one says which. Two spellings:
//
//	MAST_FAKE_PROPOSED_CHANGE='[{"tool":"x","arguments":"{\"a\":1}"}]'
//	MAST_FAKE_PROPOSED_CHANGE=x     // shorthand for tool x, no arguments
//
// The shorthand exists for the refusal legs, where the point is a tool
// name mast will not recognize and the arguments are beside the point.
const fakeProposedChangeEnv = "MAST_FAKE_PROPOSED_CHANGE"

// changeSetProperty is the report property that carries the change set.
//
// Spelled here rather than imported from pkg/approval, which owns it:
// this package's offline fakes are reached from that package's own
// tests, and importing back would be a cycle. TestChangeSetPropertyName
// (schemafill_test.go, which may import approval — the cycle only bites
// non-test code) is what keeps the two spellings one name, because a
// fake filling a field nothing reads looks exactly like a passing run.
const changeSetProperty = "proposed_change"

// proposedChange returns the value the fakes put in the change-set
// field.
func proposedChange() []any {
	spec := strings.TrimSpace(os.Getenv(fakeProposedChangeEnv))
	if spec == "" {
		return []any{}
	}
	if strings.HasPrefix(spec, "[") {
		var out []any
		if err := json.Unmarshal([]byte(spec), &out); err == nil {
			return out
		}
		// Fall through: a malformed JSON array is a harness bug, and
		// silently sending an empty list would report it as a passing
		// run. Sending the raw text as a tool name fails loudly instead.
	}
	return []any{map[string]any{"tool": spec, "arguments": "{}"}}
}

// finishTaskParams digs the finish_task parameter schema out of the
// request config — the declaration as it goes on the wire, which is
// what the runtime validates against. Nil when finish_task is not
// declared with parameters.
func finishTaskParams(req *model.LLMRequest) *genai.Schema {
	if req == nil || req.Config == nil {
		return nil
	}
	for _, t := range req.Config.Tools {
		if t == nil {
			continue
		}
		for _, fd := range t.FunctionDeclarations {
			if fd != nil && fd.Name == "finish_task" {
				return fd.Parameters
			}
		}
	}
	return nil
}

// responseSchema digs the forced structured-output schema out of the
// request config. Nil unless the runtime asked for one.
//
// This is the *other* way a declared output_schema reaches the wire.
// A toolless SingleTurn specialist (W4.3's bounded shape) gets no
// finish_task to hang the contract off, so ADK's basic processor puts
// the schema on the request itself — Config.ResponseSchema plus
// ResponseMIMEType: application/json — and validates the reply text
// against it when the turn ends. A fake that answered such a request
// with the usual "[echo] acknowledged: ..." line failed that check
// every time, so the bounded path could not be exercised offline at
// all: the run died on output-schema validation rather than on
// anything the shape got wrong. Filling this in is what lets
// U-bounded-cost run without a provider.
func responseSchema(req *model.LLMRequest) *genai.Schema {
	if req == nil || req.Config == nil {
		return nil
	}
	return req.Config.ResponseSchema
}

// structuredReply renders the JSON document a forced-structured-output
// request demands, using the same property filler the finish_task path
// uses so both spellings of "answer the contract you were handed"
// produce the same report for the same schema.
//
// Reports false when no object schema is in force, which leaves every
// existing fake path byte-identical. Non-object response schemas fall
// through deliberately: mast's report contract is an object, and a fake
// that guessed at a bare array or scalar would be inventing a shape no
// shipped bundle asks for.
func structuredReply(req *model.LLMRequest, seed string) (string, bool) {
	decl := responseSchema(req)
	if decl == nil || schemaType(decl) != genai.TypeObject {
		return "", false
	}
	args := conformingArgs(decl, seed)
	if violateSchemaEnabled() && len(decl.Required) > 0 {
		// The violation knob has to reach this path too, or the harness
		// leg that proves the contract is enforced would quietly pass by
		// never being applied. There is no giving-up turn to pair with
		// it here: a SingleTurn agent gets one turn, so the violation is
		// terminal by construction.
		delete(args, decl.Required[0])
	}
	out, err := json.Marshal(args)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// isDefaultResultWrapper reports whether s is ADK's stand-in schema for
// a Task agent with no output_schema: an object carrying exactly one
// string property named "result" (internal/workflowinternal's
// defaultWrapperKey). Recognizing it is what keeps the unschema'd path
// byte-identical to the fakes' historical behavior.
func isDefaultResultWrapper(s *genai.Schema) bool {
	if s == nil || len(s.Properties) != 1 {
		return false
	}
	p, ok := s.Properties["result"]
	return ok && schemaType(p) == genai.TypeString
}

// schemaType normalizes a schema's type, which arrives upper-cased on
// the wire ("STRING") but may be written either way in Go.
func schemaType(s *genai.Schema) genai.Type {
	if s == nil {
		return genai.TypeUnspecified
	}
	return genai.Type(strings.ToUpper(string(s.Type)))
}

// finishTaskArgs builds arguments for a finish_task call that satisfy
// whatever schema the runtime declared for it.
//
// With no output schema declared (ADK's default `result` wrapper), the
// caller's fallback text is used unchanged. With one declared, every
// property is filled from its type: enums take their first value, so
// the output is deterministic run to run, and every free string carries
// the seed (the incident reason) so a report can be traced back to the
// incident that produced it.
//
// Under MAST_FAKE_SCHEMA_VIOLATION the first required property is
// omitted instead — but only when a real output schema is in force.
// Dropping "result" from the default wrapper would exercise ADK's
// baseline validation, not the workload's report contract, and the
// point of the switch is the latter.
func finishTaskArgs(req *model.LLMRequest, seed, fallbackResult string) map[string]any {
	decl := finishTaskParams(req)
	if decl == nil || isDefaultResultWrapper(decl) {
		return map[string]any{"result": fallbackResult}
	}
	args := conformingArgs(decl, seed)
	if violateSchemaEnabled() && len(decl.Required) > 0 {
		delete(args, decl.Required[0])
	}
	return args
}

// conformingArgs fills every declared property of an object schema.
func conformingArgs(s *genai.Schema, seed string) map[string]any {
	out := make(map[string]any, len(s.Properties))
	for name, prop := range s.Properties {
		if name == changeSetProperty && schemaType(prop) == genai.TypeArray {
			out[name] = proposedChange()
			continue
		}
		out[name] = sampleValue(prop, seed, name)
	}
	return out
}

// sampleValue synthesizes one schema-conforming value. Arrays get
// exactly one element: an empty array satisfies most schemas but says
// nothing about the item type, and a report whose `recommended_actions`
// is always empty is a weaker fixture than one that is not.
func sampleValue(s *genai.Schema, seed, name string) any {
	switch schemaType(s) {
	case genai.TypeString:
		if len(s.Enum) > 0 {
			return s.Enum[0]
		}
		return fakeText(seed, name)
	case genai.TypeInteger:
		return 1
	case genai.TypeNumber:
		return 1.0
	case genai.TypeBoolean:
		return false
	case genai.TypeArray:
		if s.Items == nil {
			return []any{}
		}
		return []any{sampleValue(s.Items, seed, name)}
	case genai.TypeObject:
		return conformingArgs(s, seed)
	default:
		return fakeText(seed, name)
	}
}

// fakeText is the filler for an unconstrained string. It names both the
// fake and the incident so a report that reaches an operator's screen
// can never be mistaken for a real diagnosis.
func fakeText(seed, name string) string {
	if seed == "" {
		return fmt.Sprintf("[fake] %s", name)
	}
	return fmt.Sprintf("[fake:%s] %s", seed, name)
}

// schemaViolationGiveUp reports whether a fake in violation mode should
// stop calling finish_task and end its turn with plain text — which
// ends a Task agent without a result.
//
// A stateless fake that violated the contract on every call would spin
// until some budget killed it, turning a contract test into a timeout.
// Giving up once the refusal is visible in the history bounds the leg
// and makes its outcome specific: no report, rather than no report and
// a budget error to explain away.
func schemaViolationGiveUp(req *model.LLMRequest) bool {
	if !violateSchemaEnabled() || req == nil {
		return false
	}
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != "finish_task" {
				continue
			}
			if _, bad := p.FunctionResponse.Response["error"]; bad {
				return true
			}
		}
	}
	return false
}
