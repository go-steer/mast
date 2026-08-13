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
	"strings"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
)

// reqWithFinishTask builds a request whose config declares finish_task
// with params — the shape ADK puts on the wire, which is the only place
// a model can learn the report contract.
func reqWithFinishTask(params *genai.Schema) *model.LLMRequest {
	return &model.LLMRequest{
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{Name: "some_other_tool", Parameters: &genai.Schema{Type: genai.TypeObject}},
					{Name: "finish_task", Parameters: params},
				},
			}},
		},
	}
}

// defaultWrapper is ADK's stand-in declaration for a Task agent with no
// output_schema (internal/workflowinternal/finish_task_tool.go).
func defaultWrapper() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"result": {Type: genai.TypeString, Description: "A brief summary of what the agent accomplished."},
		},
		Required: []string{"result"},
	}
}

// findingSchema mirrors the shape of the shipped
// examples/workloads/gke-triage/schemas/finding.json closely enough to
// cover every filler branch: an enum, free strings, an array of
// strings, and a boolean.
func findingSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"severity":            {Type: genai.TypeString, Enum: []string{"critical", "warning", "info"}},
			"title":               {Type: genai.TypeString},
			"detail":              {Type: genai.TypeString},
			"reason":              {Type: genai.TypeString},
			"recommended_actions": {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}},
			"escalate":            {Type: genai.TypeBoolean},
		},
		Required: []string{"severity", "title", "detail", "reason"},
	}
}

// TestFinishTaskArgs_DefaultWrapperKeepsCallerText pins the unschema'd
// path. Most of mast's fixtures declare no output_schema and their
// digests are asserted verbatim elsewhere (mast_test.go), so the filler
// must recognize ADK's default wrapper and stay out of the way.
func TestFinishTaskArgs_DefaultWrapperKeepsCallerText(t *testing.T) {
	got := finishTaskArgs(reqWithFinishTask(defaultWrapper()), "OOMKilled", "[echo triage] diagnosed")
	if len(got) != 1 || got["result"] != "[echo triage] diagnosed" {
		t.Errorf("args = %v, want the caller's text under result", got)
	}
}

// TestFinishTaskArgs_NoDeclarationKeepsCallerText covers the model that
// is handed no declaration at all (a fake driven directly in a unit
// test, with no Config on the request).
func TestFinishTaskArgs_NoDeclarationKeepsCallerText(t *testing.T) {
	got := finishTaskArgs(&model.LLMRequest{}, "OOMKilled", "fallback")
	if len(got) != 1 || got["result"] != "fallback" {
		t.Errorf("args = %v, want the caller's text under result", got)
	}
}

// TestFinishTaskArgs_FillsDeclaredSchema is the core claim: a declared
// report contract is answered on its own terms. Every property is
// filled, an enum resolves to a fixed member (determinism run to run),
// an array carries a typed element, and every free string names the
// incident so a report is traceable.
func TestFinishTaskArgs_FillsDeclaredSchema(t *testing.T) {
	got := finishTaskArgs(reqWithFinishTask(findingSchema()), "OOMKilled", "unused")

	if _, leaked := got["result"]; leaked {
		t.Errorf("the default wrapper key leaked into a schema'd call: %v", got)
	}
	for _, key := range findingSchema().Required {
		if _, ok := got[key]; !ok {
			t.Errorf("required property %q missing from %v", key, got)
		}
	}
	if got["severity"] != "critical" {
		t.Errorf("severity = %v, want the first enum member", got["severity"])
	}
	if got["escalate"] != false {
		t.Errorf("escalate = %v, want a boolean", got["escalate"])
	}
	acts, ok := got["recommended_actions"].([]any)
	if !ok || len(acts) != 1 {
		t.Fatalf("recommended_actions = %v, want a one-element array", got["recommended_actions"])
	}
	if s, ok := acts[0].(string); !ok || !strings.Contains(s, "OOMKilled") {
		t.Errorf("array element = %v, want a string naming the incident", acts[0])
	}
	title, _ := got["title"].(string)
	if !strings.Contains(title, "OOMKilled") {
		t.Errorf("title = %q, want the incident reason in it", title)
	}
	if !strings.Contains(title, "fake") {
		t.Errorf("title = %q, want it to announce itself as fake output", title)
	}
}

// TestFinishTaskArgs_NestedObject covers the recursive branch. No
// shipped roster nests today, but a filler that silently emitted a bare
// string for an object property would produce a violation that reads
// like a mast bug.
func TestFinishTaskArgs_NestedObject(t *testing.T) {
	schema := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"outer": {
				Type:       genai.TypeObject,
				Properties: map[string]*genai.Schema{"inner": {Type: genai.TypeInteger}},
			},
		},
		Required: []string{"outer"},
	}
	got := finishTaskArgs(reqWithFinishTask(schema), "seed", "unused")
	outer, ok := got["outer"].(map[string]any)
	if !ok {
		t.Fatalf("outer = %T, want a nested object", got["outer"])
	}
	if outer["inner"] != 1 {
		t.Errorf("outer.inner = %v, want an integer", outer["inner"])
	}
}

// TestFinishTaskArgs_ViolationDropsFirstRequired pins the deliberate
// violation the black-box harness needs. It is the discriminating half
// of `U-report`: without a violating run reaching a visibly different
// end, "the report conforms" is a claim about the fake, not about mast.
func TestFinishTaskArgs_ViolationDropsFirstRequired(t *testing.T) {
	t.Setenv(fakeSchemaViolationEnv, "1")
	got := finishTaskArgs(reqWithFinishTask(findingSchema()), "OOMKilled", "unused")
	if _, ok := got["severity"]; ok {
		t.Errorf("severity is still present under violation mode: %v", got)
	}
	// Only the one key goes: a call missing everything would be refused
	// for reasons that have nothing to do with the report contract.
	for _, key := range []string{"title", "detail", "reason"} {
		if _, ok := got[key]; !ok {
			t.Errorf("violation mode dropped %q too; only the first required key should go", key)
		}
	}
}

// TestFinishTaskArgs_ViolationSparesTheDefaultWrapper: violation mode
// targets the workload's report contract, not ADK's baseline. An
// unschema'd fixture must behave identically with the switch on, or
// every other leg of a harness that sets it would change meaning.
func TestFinishTaskArgs_ViolationSparesTheDefaultWrapper(t *testing.T) {
	t.Setenv(fakeSchemaViolationEnv, "1")
	got := finishTaskArgs(reqWithFinishTask(defaultWrapper()), "OOMKilled", "text")
	if got["result"] != "text" {
		t.Errorf("args = %v, want the unschema'd path untouched by violation mode", got)
	}
}

// refusedRequest is a history in which finish_task came back with an
// error — what the runtime sends when a call violates the schema.
func refusedRequest() *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{{
		Role: genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromFunctionResponse("finish_task", map[string]any{
			"error": `missing required property "severity"`,
		})},
	}}}
}

// TestSchemaViolationGiveUp covers the bound on the violation leg: the
// fake stops once the refusal is visible, so the leg ends in "no
// report" rather than in whichever budget ran out first.
func TestSchemaViolationGiveUp(t *testing.T) {
	if schemaViolationGiveUp(refusedRequest()) {
		t.Error("gave up with the switch off: violation mode must be opt-in")
	}

	t.Setenv(fakeSchemaViolationEnv, "1")
	if schemaViolationGiveUp(&model.LLMRequest{}) {
		t.Error("gave up before any refusal: the first call must be the violating one")
	}
	if !schemaViolationGiveUp(refusedRequest()) {
		t.Error("did not give up after a refusal: the fake would spin until a budget killed it")
	}

	// A successful finish_task response is not a refusal.
	okReq := &model.LLMRequest{Contents: []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromFunctionResponse("finish_task", map[string]any{"result": "done"})},
	}}}
	if schemaViolationGiveUp(okReq) {
		t.Error("treated a successful finish_task response as a refusal")
	}
}

// TestEchoAnswersDeclaredReportSchema drives the echo model itself, not
// just the filler: the wiring between "a schema was declared" and "the
// call satisfies it" is what the shipped gke-triage roster depended on
// and did not have.
func TestEchoAnswersDeclaredReportSchema(t *testing.T) {
	req := reqWithFinishTask(findingSchema())
	req.Tools = map[string]any{"finish_task": struct{}{}}
	req.Contents = []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromText(`INJECT {"reason":"OOMKilled","namespace":"prod"}`)},
	}}

	m := NewEchoModel("echo")
	var fc *genai.FunctionCall
	for resp, err := range m.GenerateContent(t.Context(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		for _, p := range resp.Content.Parts {
			if p.FunctionCall != nil {
				fc = p.FunctionCall
			}
		}
	}
	if fc == nil || fc.Name != "finish_task" {
		t.Fatalf("echo did not call finish_task: %v", fc)
	}
	if fc.Args["severity"] != "critical" {
		t.Errorf("args = %v, want the declared schema answered", fc.Args)
	}
	if _, leaked := fc.Args["result"]; leaked {
		t.Errorf("echo sent the unschema'd digest against a declared schema: %v", fc.Args)
	}
}

// TestToolActorAnswersDeclaredReportSchema is the same claim for the
// other fake. Both drive Task specialists, so a fix in one is a fix in
// half the paths.
func TestToolActorAnswersDeclaredReportSchema(t *testing.T) {
	req := reqWithFinishTask(findingSchema())
	req.Tools = map[string]any{"finish_task": struct{}{}}
	req.Contents = []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromText(`INJECT {"reason":"OOMKilled","namespace":"prod"}`)},
	}}

	m := NewToolActorModel("toolactor")
	var fc *genai.FunctionCall
	for resp, err := range m.GenerateContent(t.Context(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		for _, p := range resp.Content.Parts {
			if p.FunctionCall != nil {
				fc = p.FunctionCall
			}
		}
	}
	if fc == nil || fc.Name != "finish_task" {
		t.Fatalf("toolactor did not call finish_task: %v", fc)
	}
	if fc.Args["severity"] != "critical" {
		t.Errorf("args = %v, want the declared schema answered", fc.Args)
	}
}

// TestToolActorClassifiesWithBareReason pins the routing half: a
// toolless turn is mast's SingleTurn classifier, and answering it with
// prose sends every incident to the Default (_fallback) edge, which
// hides a routing regression behind a specialist that still produces a
// report.
func TestToolActorClassifiesWithBareReason(t *testing.T) {
	req := &model.LLMRequest{Contents: []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromText(`INJECT {"reason":"OOMKilled","namespace":"prod"}`)},
	}}}

	var text string
	for resp, err := range NewToolActorModel("toolactor").GenerateContent(t.Context(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		for _, p := range resp.Content.Parts {
			text += p.Text
		}
	}
	if text != "OOMKilled" {
		t.Errorf("classifier reply = %q, want the bare route key", text)
	}
}
