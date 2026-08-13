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

package specialists_test

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
)

// reportSchema is the shape the tests in this file hold specialists to.
// Two required strings and one optional — enough that "missed a
// required key" and "wrong type" are both expressible.
func reportSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"summary":  {Type: genai.TypeString},
			"severity": {Type: genai.TypeString},
			"restarts": {Type: genai.TypeInteger},
		},
		Required: []string{"summary", "severity"},
	}
}

// scriptedTaskModel answers a Task-mode specialist with a fixed
// sequence of finish_task argument maps, and records what came back for
// each. It exists because the claim under test — "a violation is an
// error, not a warning" — is a claim about ADK's behaviour, and reading
// ADK's source is not the same as watching it refuse.
type scriptedTaskModel struct {
	name string

	// args is the sequence of finish_task argument maps to send, one
	// per model call. Once exhausted the model answers with plain text,
	// which ends the task without a result — a bounded failure. Repeating
	// the last entry instead would spin forever the moment finish_task
	// stops accepting it, turning a regression into a CI hang.
	args []map[string]any

	calls int
	// decl is the finish_task parameter schema as the model saw it.
	decl *genai.Schema
	// toolErrors collects every finish_task function response that came
	// back carrying an "error" key.
	toolErrors []string
}

func (m *scriptedTaskModel) Name() string { return m.name }

func (m *scriptedTaskModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.decl = finishTaskParams(req)
		m.toolErrors = collectFinishTaskErrors(req.Contents)

		i := m.calls
		m.calls++
		resp := &model.LLMResponse{TurnComplete: true, FinishReason: genai.FinishReasonStop}
		if i >= len(m.args) {
			resp.Content = genai.NewContentFromText("out of script", genai.RoleModel)
		} else {
			resp.Content = &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall("finish_task", m.args[i])},
			}
		}
		yield(resp, nil)
	}
}

// finishTaskParams digs the finish_task parameter schema out of the
// request config — the function declaration as it would go on the wire,
// rather than the tool object mast handed ADK. If the contract only
// existed on the Go side it would constrain nothing.
func finishTaskParams(req *model.LLMRequest) *genai.Schema {
	if req.Config == nil {
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

func collectFinishTaskErrors(contents []*genai.Content) []string {
	var out []string
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != "finish_task" {
				continue
			}
			if e, ok := p.FunctionResponse.Response["error"]; ok {
				out = append(out, toString(e))
			}
		}
	}
	return out
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

// runSpecialist drives one user turn through a coordinator that
// delegates to a single specialist, and returns every event the run
// produced plus the first error. Task- and SingleTurn-mode agents
// cannot be a runner root, so a coordinator is the only way in.
func runSpecialist(t *testing.T, sub adkagent.Agent) ([]*session.Event, error) {
	t.Helper()
	root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        "coord",
		Description: "Coordinator under test.",
		Instruction: "Delegate.",
		Model:       &delegatingModel{order: []string{sub.Name()}},
		SubAgents:   []adkagent.Agent{sub},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           "outputschema-test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	var events []*session.Event
	msg := genai.NewContentFromText("hello", genai.RoleUser)
	for ev, err := range r.Run(context.Background(), "user", "s1", msg, adkagent.RunConfig{}) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// outputsFrom returns the Output of every event authored by name.
func outputsFrom(events []*session.Event, name string) []any {
	var out []any
	for _, ev := range events {
		if ev != nil && ev.Author == name && ev.Output != nil {
			out = append(out, ev.Output)
		}
	}
	return out
}

// TestBuild_TaskModeEnforcesOutputSchema is W1.3's exit criterion on
// the Task path: a specialist that declares an output schema returns a
// schema-validated object, and a violation is an error rather than a
// warning.
//
// The interesting half is the second assertion. It would be easy to
// wire OutputSchema through, watch a well-behaved model produce a
// conforming object, and call the contract enforced — when in fact
// nothing was checking. So the script sends a violation first: an
// object missing a required key. That call must come back as an error
// the model can see, and must not become the specialist's output.
func TestBuild_TaskModeEnforcesOutputSchema(t *testing.T) {
	bad := map[string]any{"summary": "api is crashlooping"}
	good := map[string]any{"summary": "api is crashlooping", "severity": "CRITICAL"}

	m := &scriptedTaskModel{name: "analyst", args: []map[string]any{bad, good}}
	spec := specialists.Spec{
		Name:         "diagnoser",
		Description:  "d",
		Instruction:  "i",
		Mode:         specialists.ModeTask,
		OutputSchema: reportSchema(),
	}
	agent, err := specialists.Build(spec, specialists.BuildOptions{Model: m})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	events, err := runSpecialist(t, agent)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The contract reached the model. A top-level object schema is used
	// as the finish_task parameters verbatim, so this is an equality
	// check on the thing the model was actually asked for.
	if m.decl == nil {
		t.Fatal("finish_task was declared without parameters: the output schema never reached the model")
	}
	if _, ok := m.decl.Properties["severity"]; !ok {
		t.Errorf("finish_task parameters do not carry the schema's properties, got %v", m.decl.Properties)
	}
	if want := []string{"summary", "severity"}; !equalStrings(m.decl.Required, want) {
		t.Errorf("finish_task required = %v, want %v", m.decl.Required, want)
	}

	// The violation was refused, and refused visibly enough for the
	// model to correct itself.
	if len(m.toolErrors) == 0 {
		t.Fatal("the finish_task call missing a required key was accepted: no error came back to the model")
	}
	if got := m.toolErrors[0]; !strings.Contains(got, "severity") {
		t.Errorf("validation error does not name the missing key: %q", got)
	}

	// And the violating object never became the result.
	outs := outputsFrom(events, "diagnoser")
	if len(outs) != 1 {
		t.Fatalf("specialist produced %d outputs, want exactly 1: %v", len(outs), outs)
	}
	got, ok := outs[0].(map[string]any)
	if !ok {
		t.Fatalf("output is %T, want a schema-validated object", outs[0])
	}
	if got["severity"] != "CRITICAL" || got["summary"] != "api is crashlooping" {
		t.Errorf("output = %v, want the conforming object", got)
	}
}

// TestBuild_TaskModeWithoutSchemaTakesAnyShape is the neutralize half
// of the test above: with no output_schema declared, the same violating
// call is accepted. Without this, a bug that made every finish_task
// call fail would still let the previous test pass.
func TestBuild_TaskModeWithoutSchemaTakesAnyShape(t *testing.T) {
	m := &scriptedTaskModel{name: "analyst", args: []map[string]any{{"result": "anything at all"}}}
	spec := specialists.Spec{Name: "loose", Description: "d", Instruction: "i", Mode: specialists.ModeTask}
	agent, err := specialists.Build(spec, specialists.BuildOptions{Model: m})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := runSpecialist(t, agent); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(m.toolErrors) != 0 {
		t.Errorf("an unconstrained specialist rejected its own output: %v", m.toolErrors)
	}
}

// replyModel answers with one fixed text reply. SingleTurn agents have
// no finish_task loop, so the reply is the output.
type replyModel struct {
	name  string
	reply string
}

func (m *replyModel) Name() string { return m.name }

func (m *replyModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
			Content:      genai.NewContentFromText(m.reply, genai.RoleModel),
		}, nil)
	}
}

// responsesTo returns the function-response payloads the caller received
// for delegations to name.
//
// This, not Event.Output, is the observation point for a delegated
// specialist. The runner clones a MessageAsOutput event and clears
// Output before yielding it (runner/run_node.go), so a delegated
// specialist's result reaches an observer only through the
// function-response the caller sees — which is also the only place the
// coordinator's own model can read it.
func responsesTo(events []*session.Event, name string) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				out = append(out, p.FunctionResponse.Response)
			}
		}
	}
	return out
}

// TestBuild_SingleTurnModeEnforcesOutputSchema pins the other path.
//
// SingleTurn has no retry loop of its own: the reply is validated on the
// way out of the agent, and a failure aborts the node. Reached the way
// mast reaches it — as a sub-agent of a coordinator — that abort becomes
// an error function-response to the caller rather than a run error, so
// the enforcement shape is the same on both paths: the violation is
// refused, named, and visible to the model that asked for it. A
// conforming reply arrives as a parsed object, not as text.
//
// Both halves are asserted, because "it was refused" is only meaningful
// next to a conforming reply that is not.
func TestBuild_SingleTurnModeEnforcesOutputSchema(t *testing.T) {
	t.Run("a violating reply is refused", func(t *testing.T) {
		spec := specialists.Spec{
			Name:         "classifier_bad",
			Description:  "d",
			Instruction:  "i",
			Mode:         specialists.ModeSingleTurn,
			OutputSchema: reportSchema(),
		}
		agent, err := specialists.Build(spec, specialists.BuildOptions{
			Model: &replyModel{name: "m", reply: `{"summary": "no severity here"}`},
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		events, err := runSpecialist(t, agent)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		resps := responsesTo(events, "classifier_bad")
		if len(resps) != 1 {
			t.Fatalf("got %d delegation responses, want 1: %v", len(resps), resps)
		}
		msg, ok := resps[0]["error"].(string)
		if !ok {
			t.Fatalf("a reply missing a required key was accepted; response = %v", resps[0])
		}
		if !strings.Contains(msg, "severity") {
			t.Errorf("refusal does not name the missing key: %q", msg)
		}
		// And the half-formed object did not come back as a result.
		if _, ok := resps[0]["result"]; ok {
			t.Errorf("a refused reply still produced a result: %v", resps[0])
		}
	})

	t.Run("a conforming reply is parsed into an object", func(t *testing.T) {
		spec := specialists.Spec{
			Name:         "classifier_good",
			Description:  "d",
			Instruction:  "i",
			Mode:         specialists.ModeSingleTurn,
			OutputSchema: reportSchema(),
		}
		agent, err := specialists.Build(spec, specialists.BuildOptions{
			Model: &replyModel{name: "m", reply: `{"summary": "ok", "severity": "INFO"}`},
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		events, err := runSpecialist(t, agent)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		resps := responsesTo(events, "classifier_good")
		if len(resps) != 1 {
			t.Fatalf("got %d delegation responses, want 1: %v", len(resps), resps)
		}
		if e, ok := resps[0]["error"]; ok {
			t.Fatalf("a conforming reply was refused: %v", e)
		}
		got, ok := resps[0]["result"].(map[string]any)
		if !ok {
			t.Fatalf("result is %T, want the parsed object (a schema'd SingleTurn agent must not hand back raw text)", resps[0]["result"])
		}
		if got["severity"] != "INFO" || got["summary"] != "ok" {
			t.Errorf("result = %v, want the parsed reply", got)
		}
	})

	// The neutralize half: with no output_schema, the same violating
	// reply is accepted and comes back as raw text. Without this, a bug
	// that failed every SingleTurn delegation would leave the refusal
	// assertion above passing for the wrong reason.
	t.Run("without a schema the same reply is accepted as text", func(t *testing.T) {
		spec := specialists.Spec{
			Name:        "classifier_loose",
			Description: "d",
			Instruction: "i",
			Mode:        specialists.ModeSingleTurn,
		}
		agent, err := specialists.Build(spec, specialists.BuildOptions{
			Model: &replyModel{name: "m", reply: `{"summary": "no severity here"}`},
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		events, err := runSpecialist(t, agent)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		resps := responsesTo(events, "classifier_loose")
		if len(resps) != 1 {
			t.Fatalf("got %d delegation responses, want 1: %v", len(resps), resps)
		}
		if e, ok := resps[0]["error"]; ok {
			t.Fatalf("an unconstrained specialist refused its own reply: %v", e)
		}
		if got, ok := resps[0]["result"].(string); !ok {
			t.Errorf("result is %T, want raw text when no schema is declared", resps[0]["result"])
		} else if !strings.Contains(got, "no severity here") {
			t.Errorf("result = %q, want the reply verbatim", got)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
