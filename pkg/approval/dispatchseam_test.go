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
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/graph"
	"github.com/go-steer/mast/pkg/router"
	"github.com/go-steer/mast/pkg/workload"
)

// adkseam_test.go pins the seam on a flat agent: one LlmAgent that owns
// the mutating tool. mast never ships that shape. Its dispatch shapes all
// put the mutating tool one level down — a Task specialist under a
// coordinator, or a Task specialist inside a workflow-graph node — and
// ADK's confirmation resume re-dispatches from whichever agent the runner
// starts, which is the ROOT. Whether that reaches the specialist's tool
// is documented nowhere, and the answer decides whether the write gate is
// one mechanism or one per shape.
//
// These two probes measure it, against mast's own builders (pkg/router,
// pkg/graph) rather than hand-rolled equivalents, so what they measure is
// what mast ships. Both answers came out the same way and neither was
// obvious:
//
//   - Coordinator: the resume reaches the specialist's tool and runs it
//     exactly once, even though the root's own tool map does not contain
//     it. The confirmation processor runs per-flow and matches on the
//     session log, so the specialist's own flow resolves it when the
//     coordinator re-delegates.
//   - Graph: the same, but by a different route and at a cost. A
//     workflowagent root recognises only adk_request_input as a resume
//     (workflow.go:133), so a confirmation resume falls through to a
//     FRESH workflow run — the graph re-executes from START. It still
//     ends with exactly one execution, because every node's LlmAgent
//     rebuilds its state from the session log and the parked call is
//     already in it. What is NOT free is the upstream nodes: they really
//     do run again. TestSeamUnderGraphDispatch asserts that too, so the
//     cost is a measured property rather than a surprise.
//
// One gate covers both shapes. That is the finding the design rests on.

// routingModel answers by what it is offered rather than by a round
// counter. One model instance drives every agent in a composed tree, and
// a resume replays turns in an order no script can predict.
type routingModel struct {
	mu    sync.Mutex
	turns int
}

func (m *routingModel) Name() string { return "routing" }

func (m *routingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.turns++
		m.mu.Unlock()

		call := func(name string, args map[string]any) {
			yield(&model.LLMResponse{
				Content: &genai.Content{
					Role:  genai.RoleModel,
					Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
				},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}, nil)
		}

		// The specialist: scale once, then finish. "Once" is judged from
		// the call, not from its response — a parked call has a response
		// ("awaiting approval"), and re-calling it would measure the
		// model's retry behaviour instead of ADK's resume.
		if _, ok := req.Tools["scale_deployment"]; ok && !calledInHistory(req, "scale_deployment") {
			call("scale_deployment", map[string]any{"deployment": "api", "replicas": 10})
			return
		}
		if _, ok := req.Tools["finish_task"]; ok {
			call("finish_task", map[string]any{"result": "remediation attempted"})
			return
		}
		// The coordinator: delegate once, then answer.
		if _, ok := req.Tools["remediator"]; ok && !calledInHistory(req, "remediator") {
			call("remediator", map[string]any{"request": "scale api to 10"})
			return
		}
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText("done", genai.RoleModel),
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil)
	}
}

// constModel always answers with the same text, and counts how often it
// was asked. The graph's router matches the classifier's output against
// specialist names, so the probe needs a classifier that names one
// exactly; the count is what shows whether a confirmation resume re-runs
// the workflow from START.
type constModel struct {
	text string
	mu   sync.Mutex
	runs int
}

func (m *constModel) Name() string { return "const" }

func (m *constModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs
}

func (m *constModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.runs++
		m.mu.Unlock()
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText(m.text, genai.RoleModel),
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil)
	}
}

func calledInHistory(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == name {
				return true
			}
		}
	}
	return false
}

// dispatchProbe is seamProbe for a composed tree: same recording, but the
// root is built by mast rather than by the test, and the second turn's
// outcome (error or not) is part of the measurement.
type dispatchProbe struct {
	executions     []scaleArgs
	confirmationID string
	verdicts       []*verdictSeen
	resumeErr      error
}

// verdictSeen is an alias-free record of what the callback saw, so
// the probe does not depend on the ADK struct staying comparable.
type verdictSeen struct {
	confirmed bool
	present   bool
}

func runDispatchProbe(t *testing.T, prompt string, build func(spec adkagent.Agent) (adkagent.Agent, error)) *dispatchProbe {
	t.Helper()
	probe := &dispatchProbe{}

	scale, err := functiontool.New(functiontool.Config{
		Name:        "scale_deployment",
		Description: "changes a deployment's replica count",
	}, func(_ adkagent.Context, args scaleArgs) (map[string]any, error) {
		probe.executions = append(probe.executions, args)
		return map[string]any{"scaled": args.Replicas}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	m := &routingModel{}
	spec, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        "remediator",
		Description: "applies remediations",
		Instruction: "remediate",
		Model:       m,
		Tools:       []tool.Tool{scale},
	})
	if err != nil {
		t.Fatalf("NewTaskAgent: %v", err)
	}

	root, err := build(spec)
	if err != nil {
		t.Fatalf("build root: %v", err)
	}

	gate := func(ctx adkagent.Context, tl tool.Tool, args map[string]any) (map[string]any, error) {
		if tl.Name() != "scale_deployment" {
			return nil, nil
		}
		c := ctx.ToolConfirmation()
		probe.verdicts = append(probe.verdicts, &verdictSeen{present: c != nil, confirmed: c != nil && c.Confirmed})
		if c == nil {
			if err := ctx.RequestConfirmation("approve "+tl.Name()+"?", map[string]any{"args": args}); err != nil {
				return nil, err
			}
			return map[string]any{"status": "awaiting operator approval"}, nil
		}
		if !c.Confirmed {
			return map[string]any{"status": "rejected by operator"}, nil
		}
		return nil, nil
	}

	p, err := plugin.New(plugin.Config{Name: "dispatch-gate", BeforeToolCallback: llmagent.BeforeToolCallback(gate)})
	if err != nil {
		t.Fatalf("plugin.New: %v", err)
	}

	svc := sqliteService(t)
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

	run := func(msg *genai.Content) error {
		var first error
		for _, err := range r.Run(context.Background(), testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil && first == nil {
				first = err
			}
		}
		return first
	}

	if err := run(genai.NewContentFromText(prompt, genai.RoleUser)); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	probe.confirmationID, _ = confirmationRequest(t, svc)
	if probe.confirmationID == "" {
		return probe
	}
	probe.resumeErr = run(verdictResponse(probe.confirmationID, map[string]any{"confirmed": true}))
	return probe
}

// TestSeamUnderCoordinatorDispatch measures the default shape: the
// mutating tool belongs to a Task specialist reached as a coordinator
// sub-agent, so the pause is raised one level below the agent the runner
// restarts on resume.
func TestSeamUnderCoordinatorDispatch(t *testing.T) {
	p := runDispatchProbe(t, "OOMKilled: scale api to 10", func(spec adkagent.Agent) (adkagent.Agent, error) {
		return router.Build(router.Config{
			Bundle: workload.Bundle{
				Name:        "w",
				Description: "write-gate dispatch probe",
				Specialists: []string{"remediator"},
			},
			Specialists: map[string]adkagent.Agent{"remediator": spec},
			Model:       &routingModel{},
		})
	})

	if p.confirmationID == "" {
		t.Fatalf("a specialist's tool call did not park: the gate cannot pause under the default dispatch shape")
	}
	if p.resumeErr != nil {
		t.Fatalf("resume: %v", p.resumeErr)
	}
	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s) across pause + approve, want exactly 1: %+v", len(p.executions), p.executions)
	}
	if got := p.executions[0]; got.Deployment != "api" || got.Replicas != 10 {
		t.Errorf("executed with %+v, want the specialist's own arguments {api 10}", got)
	}
	if len(p.verdicts) != 2 || p.verdicts[0].present || !p.verdicts[1].confirmed {
		t.Errorf("callback saw %+v, want one pre-verdict call then one carrying the approval", deref(p.verdicts))
	}
}

// TestSeamUnderGraphDispatch measures the workflow-graph shape, whose
// root is a workflowagent. Its resume detection recognises exactly one
// FunctionResponse name (adk_request_input), so a confirmation resume is
// not a resume to it — it starts a fresh workflow run. The gate still
// works, and the tool still runs exactly once; the assertion on the
// classifier's run count is what records the price.
func TestSeamUnderGraphDispatch(t *testing.T) {
	classifierModel := &constModel{text: "remediator"}
	p := runDispatchProbe(t, "remediator", func(spec adkagent.Agent) (adkagent.Agent, error) {
		classifier, err := mastagent.NewSingleTurnAgent(mastagent.SingleTurnAgentConfig{
			Name:        "classifier",
			Description: "routes",
			Instruction: "route",
			Model:       classifierModel,
		})
		if err != nil {
			return nil, err
		}
		fallback, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
			Name:        graph.FallbackName,
			Description: "catch-all",
			Instruction: "handle",
			Model:       mastagent.NewEchoModel("echo"),
		})
		if err != nil {
			return nil, err
		}
		return graph.Build(graph.Config{
			Bundle:     workload.Bundle{Name: "w"},
			Classifier: classifier,
			Specialists: map[string]graph.Specialist{
				"remediator":       {Agent: spec},
				graph.FallbackName: {Agent: fallback},
			},
		})
	})

	if p.confirmationID == "" {
		t.Fatalf("a graph node's tool call did not park: the gate cannot pause under graph dispatch")
	}
	if p.resumeErr != nil {
		t.Fatalf("resume: %v", p.resumeErr)
	}
	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s) across pause + approve, want exactly 1: %+v", len(p.executions), p.executions)
	}
	if len(p.verdicts) != 2 || p.verdicts[0].present || !p.verdicts[1].confirmed {
		t.Errorf("callback saw %+v, want one pre-verdict call then one carrying the approval", deref(p.verdicts))
	}
	// The price of the fresh-run fallthrough: the classifier — an
	// upstream node the operator's verdict has nothing to do with — runs
	// again. Two runs, not one. If ADK ever teaches workflowagent to
	// recognise a confirmation resume this drops to one and the assertion
	// fails, which is the notification mast wants.
	if got := classifierModel.count(); got != 2 {
		t.Errorf("classifier ran %d time(s) across pause + approve, want 2: a confirmation resume re-runs the workflow from START, and this is the assertion that says so", got)
	}
}

func deref(vs []*verdictSeen) []verdictSeen {
	out := make([]verdictSeen, 0, len(vs))
	for _, v := range vs {
		out = append(out, *v)
	}
	return out
}
