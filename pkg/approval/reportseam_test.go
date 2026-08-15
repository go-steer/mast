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
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
)

// Two ADK facts the change-set contract is built on, both undocumented
// upstream, both of which would fail silently if they changed:
//
//  1. finish_task is the report seam. A Task-mode specialist's structured
//     report exists as tool ARGUMENTS exactly once — on the finish_task
//     call — and a before-tool callback sees it there, before it becomes
//     the agent's result. Answering that callback with an error response
//     is not a turn failure: the model keeps going and gets to fix the
//     report. That is what "refused back to the specialist" means.
//  2. ADK's output validation refuses any key a nested object did not
//     declare. This is why a change set's `arguments` travels as a JSON
//     string: an arguments object's keys belong to whichever tool the
//     change names, so no report schema can declare them, and an
//     undeclared key is not merely unvalidated — it is rejected.

// reportProbe records what one Task-mode report run did.
type reportProbe struct {
	// toolsSeen names every tool the before-tool callback was handed.
	toolsSeen []string
	// argsSeen is the argument map for each of those calls.
	argsSeen []map[string]any
	// responses is every finish_task FunctionResponse in the durable
	// session, in order — the model's own view of whether the report was
	// accepted.
	responses []map[string]any
	// rounds is how many times the model was asked to produce content.
	rounds int
	// svc is the store the probe ran against, so a test can read the
	// durable state the gate wrote.
	svc adksession.Service
}

// argsFor is the arguments of every call to one named tool. The gate
// also sees the coordinator's call to the specialist, so a test that
// wants the report has to say which call it means.
func (p *reportProbe) argsFor(name string) []map[string]any {
	var out []map[string]any
	for i, seen := range p.toolsSeen {
		if seen == name {
			out = append(out, p.argsSeen[i])
		}
	}
	return out
}

// callOnce is a coordinator model: it calls the named sub-agent once,
// then ends its turn. A Task agent cannot be a runner's root ("must be a
// chat LlmAgent"), which is the shape mast runs specialists in anyway.
type callOnce struct{ target string }

func (callOnce) Name() string { return "call-once" }

func (m callOnce) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		resp := &model.LLMResponse{
			Content:      genai.NewContentFromText("done", genai.RoleModel),
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}
		if _, ok := req.Tools[m.target]; ok && !calledInHistory(req, m.target) {
			resp.Content = &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall(m.target,
					map[string]any{"request": "diagnose"})},
			}
		}
		yield(resp, nil)
	}
}

// runReportProbe builds a Task-mode specialist whose output_schema is
// schema, scripts its model to answer with one finish_task call carrying
// report, and runs it under gate as a coordinator's sub-agent.
//
// The specialist is built from llmagent directly rather than through
// pkg/agent's NewTaskAgent: pkg/agent's offline fakes fill the
// change-set field, so pkg/agent's non-test code must not import this
// package, and this test's non-test counterpart is what pins the two
// spellings together (pkg/agent's TestChangeSetPropertyName).
func runReportProbe(t *testing.T, schema *genai.Schema, report map[string]any, gate llmagent.BeforeToolCallback) *reportProbe {
	t.Helper()
	probe := &reportProbe{}

	m := &scriptedModel{name: "report", calls: []*model.LLMResponse{{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(FinishTaskToolName, report)},
		},
	}}}

	spec, err := llmagent.New(llmagent.Config{
		Name:         "reporter",
		Description:  "returns a structured report",
		Instruction:  "report",
		Model:        m,
		OutputSchema: schema,
		Mode:         llmagent.ModeTask,
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	root, err := llmagent.New(llmagent.Config{
		Name:        "coordinator",
		Description: "report seam probe",
		Instruction: "dispatch",
		Model:       callOnce{target: "reporter"},
		SubAgents:   []adkagent.Agent{spec},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	p, err := plugin.New(plugin.Config{
		Name: "report-gate",
		BeforeToolCallback: func(ctx adkagent.Context, tl tool.Tool, args map[string]any) (map[string]any, error) {
			probe.toolsSeen = append(probe.toolsSeen, tl.Name())
			probe.argsSeen = append(probe.argsSeen, args)
			if gate == nil {
				return nil, nil
			}
			return gate(ctx, tl, args)
		},
	})
	if err != nil {
		t.Fatalf("plugin.New: %v", err)
	}

	svc := sqliteService(t)
	probe.svc = svc
	r, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{p}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	for _, err := range r.Run(context.Background(), testUser, sid,
		genai.NewContentFromText("diagnose", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
	}

	m.mu.Lock()
	probe.rounds = m.round
	m.mu.Unlock()
	probe.responses = finishTaskResponses(t, svc)
	return probe
}

// finishTaskResponses reads every finish_task function response out of
// the durable session — what the model was told about its report.
func finishTaskResponses(t *testing.T, svc adksession.Service) []map[string]any {
	t.Helper()
	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var out []map[string]any
	for ev := range got.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part == nil || part.FunctionResponse == nil {
				continue
			}
			if part.FunctionResponse.Name != FinishTaskToolName {
				continue
			}
			out = append(out, part.FunctionResponse.Response)
		}
	}
	return out
}

// reportSchema is a two-property report contract: one free string, and
// one nested object with exactly one declared property. The nested
// object is the shape a change set's entry would have if `arguments`
// were declared as an object rather than a string.
func reportSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"detail": {Type: genai.TypeString},
			"change": {
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"tool": {Type: genai.TypeString},
				},
			},
		},
		Required: []string{"detail"},
	}
}

func responseText(resp map[string]any) string {
	var b strings.Builder
	for k, v := range resp {
		b.WriteString(k)
		b.WriteString("=")
		if s, ok := v.(string); ok {
			b.WriteString(s)
		} else {
			b.WriteString("?")
		}
		b.WriteString(" ")
	}
	return b.String()
}

// TestFinishTaskIsTheReportSeam pins fact 1, in three parts: the
// callback is handed finish_task by that exact name, its arguments are
// the structured report itself, and an error response sends the model
// back to work rather than ending the run.
//
// FinishTaskToolName is a constant in this package because ADK keeps its
// own in internal/workflowinternal. This test is what makes that copy
// safe: a rename upstream fails here instead of silently disabling the
// producer contract, which would otherwise look exactly like "no
// specialist ever proposed a change".
func TestFinishTaskIsTheReportSeam(t *testing.T) {
	// The report contract a change-set-producing roster declares: the
	// change set travels as a list of {tool, arguments}, arguments as a
	// JSON string (see TestADKRefusesUndeclaredKeysInNestedObjects for
	// why it cannot be an object).
	schema := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"detail":         {Type: genai.TypeString},
			"recommendation": {Type: genai.TypeString},
			ChangeSetField: {Type: genai.TypeArray, Items: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"tool":      {Type: genai.TypeString},
					"arguments": {Type: genai.TypeString},
				},
				Required: []string{"tool", "arguments"},
			}},
		},
		Required: []string{"detail"},
	}
	report := map[string]any{
		"detail":         "api is OOMKilled",
		ChangeSetField:   []any{map[string]any{"tool": "patch_k8s_resource", "arguments": "{}"}},
		"recommendation": "raise the limit",
	}

	t.Run("the callback sees the report as arguments", func(t *testing.T) {
		p := runReportProbe(t, schema, report, nil)

		args := p.argsFor(FinishTaskToolName)
		if len(args) != 1 {
			t.Fatalf("before-tool callback saw %v, want one %s among them — the report seam is gone and nothing would check a change set",
				p.toolsSeen, FinishTaskToolName)
		}
		got, err := ParseChangeSet(args[0])
		if err != nil {
			t.Fatalf("ParseChangeSet over the arguments the callback saw: %v", err)
		}
		if len(got) != 1 || got[0].Tool != "patch_k8s_resource" {
			t.Fatalf("change set read back as %+v, want the one the model sent — the callback's args are not the report", got)
		}
	})

	t.Run("an error response is refused back to the model", func(t *testing.T) {
		refuse := func(_ adkagent.Context, _ tool.Tool, _ map[string]any) (map[string]any, error) {
			return map[string]any{"error": "invalid_proposed_change", "detail": "fix it"}, nil
		}
		p := runReportProbe(t, schema, report, refuse)

		// Two rounds: the report, then the turn the model gets to fix it.
		// One round would mean the refusal ended the specialist, which is
		// a stalled Task agent rather than a corrected report.
		if p.rounds < 2 {
			t.Fatalf("model was asked %d time(s), want at least 2 — an error response ended the run instead of sending the report back", p.rounds)
		}
		if len(p.responses) != 1 {
			t.Fatalf("finish_task responses = %d, want 1: %v", len(p.responses), p.responses)
		}
		if _, bad := p.responses[0]["error"]; !bad {
			t.Fatalf("the model was told %v, want the refusal — the callback's response is not what reaches it", p.responses[0])
		}
	})

	t.Run("no response accepts the report", func(t *testing.T) {
		p := runReportProbe(t, schema, report, nil)

		if p.rounds != 1 {
			t.Fatalf("model was asked %d time(s), want exactly 1 — an accepted report should end the specialist", p.rounds)
		}
		if len(p.responses) != 1 {
			t.Fatalf("finish_task responses = %d, want 1: %v", len(p.responses), p.responses)
		}
		if _, bad := p.responses[0]["error"]; bad {
			t.Fatalf("an unrefused report came back as %v, want success", p.responses[0])
		}
	})
}

// TestADKRefusesUndeclaredKeysInNestedObjects pins fact 2 — the reason
// ChangeSetField's `arguments` is a JSON string on the wire.
//
// If this ever fails because ADK started tolerating undeclared keys, the
// string encoding is still correct (mast's own roster loader refuses a
// propertyless object property for the same reason: it would accept
// anything). What would have changed is one of the two arguments for it,
// and the comment on ProposedChange.UnmarshalJSON should say so.
func TestADKRefusesUndeclaredKeysInNestedObjects(t *testing.T) {
	// Control: only declared keys, at both levels.
	ok := runReportProbe(t, reportSchema(), map[string]any{
		"detail": "api is OOMKilled",
		"change": map[string]any{"tool": "patch_k8s_resource"},
	}, nil)
	if len(ok.responses) != 1 {
		t.Fatalf("control: finish_task responses = %v, want 1", ok.responses)
	}
	if _, bad := ok.responses[0]["error"]; bad {
		t.Fatalf("control: a conforming report was refused (%v) — the probe is testing the wrong thing",
			responseText(ok.responses[0]))
	}

	// The same report with one undeclared key inside the nested object.
	// Nothing at the top level changes, so a validator that only checked
	// the outer object would accept this.
	bad := runReportProbe(t, reportSchema(), map[string]any{
		"detail": "api is OOMKilled",
		"change": map[string]any{"tool": "patch_k8s_resource", "namespace": "prod"},
	}, nil)
	if len(bad.responses) == 0 {
		t.Fatalf("no finish_task response at all")
	}
	if _, refused := bad.responses[0]["error"]; !refused {
		t.Fatalf("an undeclared nested key was ACCEPTED (%v) — if a free-form arguments object validates, the JSON-string encoding on ProposedChange is carrying an argument that no longer holds",
			responseText(bad.responses[0]))
	}
	if !strings.Contains(strings.ToLower(responseText(bad.responses[0])), "namespace") {
		t.Errorf("refusal does not name the offending key: %v", bad.responses[0])
	}
}
