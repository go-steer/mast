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

package planner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/planner"
)

// scriptedModel is a model.LLM whose reply per call is computed by a
// script over the request. Every response synthesizes UsageMetadata
// (like real models) so budget metering can be asserted, and every
// request is recorded so tests can inspect what the planner saw —
// tool declarations, function responses, system instruction.
type scriptedModel struct {
	name   string
	script func(req *model.LLMRequest) *model.LLMResponse

	mu   sync.Mutex
	reqs []*model.LLMRequest
}

func (m *scriptedModel) Name() string { return m.name }

func (m *scriptedModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.reqs = append(m.reqs, req)
		m.mu.Unlock()
		resp := m.script(req)
		if resp.UsageMetadata == nil {
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     100,
				CandidatesTokenCount: 20,
				TotalTokenCount:      120,
			}
		}
		resp.TurnComplete = true
		resp.FinishReason = genai.FinishReasonStop
		yield(resp, nil)
	}
}

func (m *scriptedModel) requests() []*model.LLMRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*model.LLMRequest(nil), m.reqs...)
}

func callResponse(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
		},
	}
}

// functionResponses flattens every FunctionResponse visible in a
// request's contents, keyed by function name.
func functionResponses(req *model.LLMRequest) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil {
				out[p.FunctionResponse.Name] = append(out[p.FunctionResponse.Name], p.FunctionResponse.Response)
			}
		}
	}
	return out
}

// planScript emits one scripted tool call per model round — chosen by
// how many tool calls this model has already emitted — and finish_task
// after the list is exhausted.
func planScript(m *scriptedModel, calls ...*model.LLMResponse) func(*model.LLMRequest) *model.LLMResponse {
	round := 0
	return func(req *model.LLMRequest) *model.LLMResponse {
		defer func() { round++ }()
		if round < len(calls) {
			return calls[round]
		}
		return callResponse("finish_task", map[string]any{"result": "planner done"})
	}
}

// specialistScript completes any Task-mode run via finish_task and
// answers SingleTurn requests with plain text.
func specialistScript(req *model.LLMRequest) *model.LLMResponse {
	if _, ok := req.Tools["finish_task"]; ok {
		return callResponse("finish_task", map[string]any{"result": "diagnosed: image tag not found"})
	}
	return &model.LLMResponse{Content: genai.NewContentFromText("classified", genai.RoleModel)}
}

func buildSpecialist(t *testing.T, name string, m model.LLM) adkagent.Agent {
	t.Helper()
	a, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        name,
		Description: name + " (test specialist)",
		Instruction: "test",
		Model:       m,
	})
	if err != nil {
		t.Fatalf("NewTaskAgent(%q): %v", name, err)
	}
	return a
}

// runPlanner drives one user turn through a runner over the given
// root, returning the streamed events.
func runPlanner(t *testing.T, r *runner.Runner, sessionID string, msg *genai.Content) []*session.Event {
	t.Helper()
	var events []*session.Event
	for ev, err := range r.Run(context.Background(), "op", sessionID, msg, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

func newRunner(t *testing.T, root adkagent.Agent, svc session.Service) *runner.Runner {
	t.Helper()
	r, err := runner.New(runner.Config{
		AppName:           "planner-test",
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

func TestNewValidation(t *testing.T) {
	if _, err := planner.New(planner.Config{Model: &scriptedModel{name: "m"}}); err == nil {
		t.Error("New without Name should fail")
	}
	if _, err := planner.New(planner.Config{Name: "w"}); err == nil {
		t.Error("New without Model should fail")
	}
}

func TestPlannerInvokesSpecialistThenFinishes(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{
			"name":  "ImagePullBackOff",
			"input": "pod web-1 stuck pulling image",
		}),
	)

	root, err := planner.NewRoot(planner.Config{
		Name:  "gke-triage",
		Model: plModel,
		Specialists: map[string]adkagent.Agent{
			"ImagePullBackOff": buildSpecialist(t, "ImagePullBackOff", spModel),
		},
		Order: []string{"ImagePullBackOff"},
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}

	r := newRunner(t, root, session.InMemoryService())
	events := runPlanner(t, r, "s1", genai.NewContentFromText("INJECT pod web-1 ImagePullBackOff", genai.RoleUser))

	if len(spModel.requests()) == 0 {
		t.Fatal("specialist model was never called — invoke_specialist did not dispatch")
	}

	// The planner's second round must have seen the specialist result
	// through the invoke_specialist function response.
	reqs := plModel.requests()
	if len(reqs) < 2 {
		t.Fatalf("planner model rounds = %d, want >= 2", len(reqs))
	}
	frs := functionResponses(reqs[len(reqs)-1])[planner.ToolInvokeSpecialist]
	if len(frs) == 0 {
		t.Fatalf("no %s FunctionResponse in the planner's final round", planner.ToolInvokeSpecialist)
	}
	if got := fmt.Sprint(frs[0]["result"]); !strings.Contains(got, "diagnosed") {
		t.Errorf("invoke_specialist result = %q, want the specialist's finish_task output", got)
	}
	if got := frs[0]["specialist"]; got != "ImagePullBackOff" {
		t.Errorf("invoke_specialist specialist = %v, want ImagePullBackOff", got)
	}

	// The turn ends with the planner's finish_task output as the
	// terminal node output.
	var lastOutput any
	for _, ev := range events {
		if ev != nil && ev.Output != nil {
			lastOutput = ev.Output
		}
	}
	if got := fmt.Sprint(lastOutput); !strings.Contains(got, "planner done") {
		t.Errorf("terminal output = %v, want the planner's finish_task result", lastOutput)
	}
}

func TestInvokeSpecialistUnknownNameIsStructuredError(t *testing.T) {
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "nope", "input": "x"}),
	)
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}

	root, err := planner.NewRoot(planner.Config{
		Name:        "w",
		Model:       plModel,
		Specialists: map[string]adkagent.Agent{"real": buildSpecialist(t, "real", spModel)},
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())
	runPlanner(t, r, "s1", genai.NewContentFromText("work", genai.RoleUser))

	reqs := plModel.requests()
	frs := functionResponses(reqs[len(reqs)-1])[planner.ToolInvokeSpecialist]
	if len(frs) == 0 {
		t.Fatal("no invoke_specialist FunctionResponse recorded")
	}
	if frs[0]["error"] != "unknown_specialist" {
		t.Errorf("error = %v, want unknown_specialist; full response: %v", frs[0]["error"], frs[0])
	}
	if got := fmt.Sprint(frs[0]["available"]); !strings.Contains(got, "real") {
		t.Errorf("available roster missing from error: %v", frs[0])
	}
}

func TestRunShapeToolsReturnNotImplemented(t *testing.T) {
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolRunShapeLLMRouter, map[string]any{
			"classifier": "clf", "handlers": []string{"a", "b"},
		}),
		callResponse(planner.ToolRunShapeFanOutFanIn, map[string]any{
			"planner_fn": "split", "workers": []string{"a"}, "joiner": "j",
		}),
	)

	root, err := planner.NewRoot(planner.Config{Name: "w", Model: plModel})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())
	runPlanner(t, r, "s1", genai.NewContentFromText("work", genai.RoleUser))

	reqs := plModel.requests()
	final := functionResponses(reqs[len(reqs)-1])
	for _, name := range []string{planner.ToolRunShapeLLMRouter, planner.ToolRunShapeFanOutFanIn} {
		frs := final[name]
		if len(frs) == 0 {
			t.Errorf("no FunctionResponse for %s", name)
			continue
		}
		want := planner.NotImplemented(name)
		if frs[0]["error"] != want["error"] || frs[0]["status"] != want["status"] || frs[0]["tool"] != want["tool"] {
			t.Errorf("%s response = %v, want structured not_implemented %v", name, frs[0], want)
		}
	}
}

// TestVocabularySnapshot pins the v0.1 planner tool contract — the
// tool names and each declaration's parameter names + required list —
// as the planner's LLM actually receives them. A change here is a
// vocabulary change and must be deliberate (docs/orchestration-design
// tool-vocabulary table).
func TestVocabularySnapshot(t *testing.T) {
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel) // finish immediately

	root, err := planner.NewRoot(planner.Config{Name: "w", Model: plModel})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())
	runPlanner(t, r, "s1", genai.NewContentFromText("work", genai.RoleUser))

	reqs := plModel.requests()
	if len(reqs) == 0 {
		t.Fatal("planner model never called")
	}
	var names []string
	for name := range reqs[0].Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	wantNames := []string{
		"finish_task",
		planner.ToolInvokeSpecialist,
		planner.ToolRequestOperatorInput,
		planner.ToolRunShapeFanOutFanIn,
		planner.ToolRunShapeLLMRouter,
	}
	sort.Strings(wantNames)
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("vocabulary = %v, want %v", names, wantNames)
	}

	wantParams := map[string]struct {
		properties []string
		required   []string
	}{
		planner.ToolInvokeSpecialist:     {[]string{"input", "name"}, []string{"name", "input"}},
		planner.ToolRunShapeLLMRouter:    {[]string{"classifier", "handlers"}, []string{"classifier", "handlers"}},
		planner.ToolRunShapeFanOutFanIn:  {[]string{"joiner", "planner_fn", "workers"}, []string{"planner_fn", "workers", "joiner"}},
		planner.ToolRequestOperatorInput: {[]string{"message", "schema"}, []string{"message"}},
	}
	for name, want := range wantParams {
		decl := declarationOf(t, reqs[0].Tools[name], name)
		props, required := paramsOf(t, decl)
		if !reflect.DeepEqual(props, want.properties) {
			t.Errorf("%s properties = %v, want %v", name, props, want.properties)
		}
		if !reflect.DeepEqual(required, want.required) {
			t.Errorf("%s required = %v, want %v", name, required, want.required)
		}
	}
}

func declarationOf(t *testing.T, tl any, name string) *genai.FunctionDeclaration {
	t.Helper()
	d, ok := tl.(interface {
		Declaration() *genai.FunctionDeclaration
	})
	if !ok {
		t.Fatalf("tool %s (%T) has no Declaration()", name, tl)
	}
	decl := d.Declaration()
	if decl == nil {
		t.Fatalf("tool %s Declaration() is nil", name)
	}
	return decl
}

// paramsOf extracts sorted property names and the declared required
// list from a declaration's ParametersJsonSchema.
func paramsOf(t *testing.T, decl *genai.FunctionDeclaration) (properties, required []string) {
	t.Helper()
	raw, err := json.Marshal(decl.ParametersJsonSchema)
	if err != nil {
		t.Fatalf("marshal ParametersJsonSchema for %s: %v", decl.Name, err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal ParametersJsonSchema for %s: %v", decl.Name, err)
	}
	for p := range schema.Properties {
		properties = append(properties, p)
	}
	sort.Strings(properties)
	return properties, schema.Required
}

// TestPlannerModelCallsAreMetered asserts the budget-composition
// contract from docs/orchestration-design.md: the planner introduces
// no metering of its own because its model calls stream past the
// runner event consumer with UsageMetadata like any other agent's —
// the same pkg/budget meter cmd/mast wires for coordinator/graph
// dispatch bounds the planner unchanged.
func TestPlannerModelCallsAreMetered(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
	)

	root, err := planner.NewRoot(planner.Config{
		Name:        "w",
		Model:       plModel,
		Specialists: map[string]adkagent.Agent{"sp": buildSpecialist(t, "sp", spModel)},
	})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	r := newRunner(t, root, session.InMemoryService())

	meter := budget.NewMeter(budget.Limits{RatePer1K: 1.0})
	for ev, err := range r.Run(context.Background(), "op", "s1",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		if berr := meter.Observe(ev); berr != nil {
			t.Fatalf("unexpected budget trip: %v", berr)
		}
	}

	tokens, cost, calls := meter.Snapshot()
	// Two planner rounds (invoke + finish) must be visible. The
	// specialist's own calls ran under the tool's private sub-runner
	// and are NOT on this stream — they reach a host through
	// Config.SubRunObserver, which is left unwired here so this test
	// keeps measuring exactly what the OUTER stream carries. #226 and
	// subrun_test.go cover the other door.
	if wantCalls := len(plModel.requests()); calls != wantCalls {
		t.Errorf("metered calls = %d, want %d (one per planner model round)", calls, wantCalls)
	}
	if calls < 2 {
		t.Errorf("metered calls = %d, want >= 2 (invoke round + finish round)", calls)
	}
	if tokens <= 0 || cost <= 0 {
		t.Errorf("metered tokens/cost = %d/$%.4f, want > 0", tokens, cost)
	}

	// And the ceiling actually binds: the same multi-round turn
	// against a fresh planner and a 1-call cap trips ErrExceeded.
	spModel2 := &scriptedModel{name: "sp-model-2", script: specialistScript}
	plModel2 := &scriptedModel{name: "pl-model-2"}
	plModel2.script = planScript(plModel2,
		callResponse(planner.ToolInvokeSpecialist, map[string]any{"name": "sp", "input": "x"}),
	)
	root2, err := planner.NewRoot(planner.Config{
		Name:        "w2",
		Model:       plModel2,
		Specialists: map[string]adkagent.Agent{"sp": buildSpecialist(t, "sp2", spModel2)},
	})
	if err != nil {
		t.Fatalf("NewRoot (capped): %v", err)
	}
	r2 := newRunner(t, root2, session.InMemoryService())
	capped := budget.NewMeter(budget.Limits{RatePer1K: 1.0, MaxTurns: 1})
	var tripped bool
	for ev, err := range r2.Run(context.Background(), "op", "s2",
		genai.NewContentFromText("work", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		if berr := capped.Observe(ev); berr != nil {
			if !errors.Is(berr, budget.ErrExceeded) {
				t.Fatalf("budget error = %v, want ErrExceeded", berr)
			}
			tripped = true
			break
		}
	}
	if !tripped {
		t.Error("MaxTurns=1 never tripped over a multi-round planner turn")
	}
}

// TestRequestOperatorInputPausesAndResumes exercises the tool-level
// HITL shape end to end: the long-running call pauses the run with
// the pending function-call ID in LongRunningToolIDs, and a
// FunctionResponse targeting that ID on a later turn resumes the
// planner, which then finishes.
func TestRequestOperatorInputPausesAndResumes(t *testing.T) {
	plModel := &scriptedModel{name: "pl-model"}
	round := 0
	plModel.script = func(req *model.LLMRequest) *model.LLMResponse {
		round++
		if round == 1 {
			return callResponse(planner.ToolRequestOperatorInput, map[string]any{
				"message": "approve the rollback?",
				"schema":  map[string]any{"type": "object"},
			})
		}
		// Resume round: the operator's answer must be visible.
		frs := functionResponses(req)[planner.ToolRequestOperatorInput]
		if len(frs) == 0 {
			return callResponse("finish_task", map[string]any{"result": "ERROR: no operator response visible"})
		}
		return callResponse("finish_task", map[string]any{
			"result": fmt.Sprintf("operator said: %v", frs[0]),
		})
	}

	root, err := planner.NewRoot(planner.Config{Name: "w", Model: plModel})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	svc := session.InMemoryService()
	r := newRunner(t, root, svc)

	// Turn 1: pause.
	events := runPlanner(t, r, "s1", genai.NewContentFromText("do risky thing", genai.RoleUser))
	var interruptID string
	var finished bool
	for _, ev := range events {
		if ev == nil {
			continue
		}
		for _, id := range ev.LongRunningToolIDs {
			if id != "" {
				interruptID = id
			}
		}
		if ev.Output != nil {
			finished = true
		}
	}
	if interruptID == "" {
		t.Fatal("no LongRunningToolIDs event — request_operator_input did not pause the run")
	}
	if finished {
		t.Fatal("run produced a terminal output on the pause turn")
	}

	// Turn 2: operator resume — a FunctionResponse whose ID matches
	// the pending call (the wire shape cmd/mast's /resume sends).
	resume := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromFunctionResponse(
			planner.ToolRequestOperatorInput,
			map[string]any{"approved": true, "note": "go ahead"},
		)},
	}
	resume.Parts[0].FunctionResponse.ID = interruptID
	events = runPlanner(t, r, "s1", resume)

	var lastOutput any
	for _, ev := range events {
		if ev != nil && ev.Output != nil {
			lastOutput = ev.Output
		}
	}
	got := fmt.Sprint(lastOutput)
	if !strings.Contains(got, "operator said") || !strings.Contains(got, "go ahead") {
		t.Errorf("post-resume output = %v, want the operator's response folded into finish_task", lastOutput)
	}
}

// TestRunNodeUnavailableFromToolContext pins the ADK v2.1.0 constraint
// that forced invoke_specialist onto the agenttool-style mechanism
// (see the mechanism note in dispatch.go): workflow.RunNode from a
// functiontool body inside a TASK-mode LlmAgent fails with
// ErrInvalidRunNodeContext, because RunLLMAgentAsNode rebuilds the
// invocation context without the dynamic-node sub-scheduler. If an
// ADK upgrade makes this pass, invoke_specialist should be rewired to
// RunNode so sub-invocations inherit session, branch, and metering.
func TestRunNodeUnavailableFromToolContext(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	target := buildSpecialist(t, "target", spModel)
	targetNode, err := workflow.NewAgentNode(target, workflow.NodeConfig{})
	if err != nil {
		t.Fatalf("NewAgentNode: %v", err)
	}

	var runNodeErr error
	probe, err := functiontool.New(functiontool.Config{
		Name:        "probe_run_node",
		Description: "attempts workflow.RunNode from a tool context",
	}, func(ctx adkagent.Context, _ struct{}) (map[string]any, error) {
		_, runNodeErr = workflow.RunNode[any](ctx, targetNode, "hello")
		return map[string]any{"attempted": true}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	probeModel := &scriptedModel{name: "probe-model"}
	probeModel.script = planScript(probeModel, callResponse("probe_run_node", map[string]any{}))
	taskAgent, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        "prober",
		Description: "task-mode probe agent",
		Instruction: "probe",
		Model:       probeModel,
		Tools:       []tool.Tool{probe},
	})
	if err != nil {
		t.Fatalf("NewTaskAgent: %v", err)
	}

	// Wrap for rootability the same way NewRoot does.
	node, err := workflow.NewAgentNode(taskAgent, workflow.NodeConfig{})
	if err != nil {
		t.Fatalf("NewAgentNode(prober): %v", err)
	}
	dyn := workflow.NewDynamicNode[any, any]("run_prober",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (any, error) {
			return workflow.RunNode[any](ctx, node, nil)
		}, workflow.NodeConfig{})
	root, err := newWorkflowRoot("prober_root", dyn, taskAgent)
	if err != nil {
		t.Fatalf("workflowagent.New: %v", err)
	}

	r := newRunner(t, root, session.InMemoryService())
	runPlanner(t, r, "s1", genai.NewContentFromText("go", genai.RoleUser))

	if !errors.Is(runNodeErr, workflow.ErrInvalidRunNodeContext) {
		t.Fatalf("RunNode from tool context returned %v, want ErrInvalidRunNodeContext — if ADK now supports this, rewire invoke_specialist (see dispatch.go)", runNodeErr)
	}
	if len(spModel.requests()) != 0 {
		t.Errorf("target specialist was invoked despite ErrInvalidRunNodeContext")
	}
}

func TestInstructionTemplateAndOverride(t *testing.T) {
	spModel := &scriptedModel{name: "sp-model", script: specialistScript}
	sp := map[string]adkagent.Agent{"diag": buildSpecialist(t, "diag", spModel)}

	// Default: template rendered with workload name + roster, visible
	// to the model as the system instruction.
	plModel := &scriptedModel{name: "pl-model"}
	plModel.script = planScript(plModel)
	root, err := planner.NewRoot(planner.Config{Name: "gke-triage", Model: plModel, Specialists: sp})
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	runPlanner(t, newRunner(t, root, session.InMemoryService()), "s1",
		genai.NewContentFromText("work", genai.RoleUser))
	sys := systemText(t, plModel)
	for _, want := range []string{"gke-triage", "diag", "invoke_specialist", "finish_task"} {
		if !strings.Contains(sys, want) {
			t.Errorf("default planner instruction missing %q; got:\n%s", want, sys)
		}
	}

	// Override: used verbatim, template gone.
	ovModel := &scriptedModel{name: "ov-model"}
	ovModel.script = planScript(ovModel)
	root, err = planner.NewRoot(planner.Config{
		Name: "gke-triage", Model: ovModel, Specialists: sp,
		Instruction: "CUSTOM-PLANNER-PROMPT",
	})
	if err != nil {
		t.Fatalf("NewRoot (override): %v", err)
	}
	runPlanner(t, newRunner(t, root, session.InMemoryService()), "s1",
		genai.NewContentFromText("work", genai.RoleUser))
	sys = systemText(t, ovModel)
	if !strings.Contains(sys, "CUSTOM-PLANNER-PROMPT") {
		t.Errorf("override instruction not passed through; got:\n%s", sys)
	}
	if strings.Contains(sys, "You are the planner for") {
		t.Errorf("template leaked alongside an explicit override; got:\n%s", sys)
	}
}

func newWorkflowRoot(name string, node workflow.Node, sub adkagent.Agent) (adkagent.Agent, error) {
	return workflowagent.New(workflowagent.Config{
		Name:      name,
		Edges:     workflow.Chain(workflow.Start, node),
		SubAgents: []adkagent.Agent{sub},
	})
}

func systemText(t *testing.T, m *scriptedModel) string {
	t.Helper()
	reqs := m.requests()
	if len(reqs) == 0 {
		t.Fatal("model never called")
	}
	req := reqs[0]
	if req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		if p != nil {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
