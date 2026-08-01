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

package planner

// This file holds the two pieces of the scaffold that are shaped by
// hard ADK v2.1.0 constraints, verified against the vendored sources
// and pinned by tests in planner_test.go:
//
// 1. invoke_specialist mechanism — agenttool-style direct invocation,
//    NOT workflow.RunNode from the tool body.
//
//    workflow.RunNode requires ctx.SubScheduler() != nil, and a
//    sub-scheduler exists only on dynamic-node activation contexts
//    (workflow/dynamic_node.go installs one; workflow/scheduler.go
//    startNode explicitly nils it for static nodes). A Chat-mode
//    coordinator's tools would inherit it — runChat drives Agent.Run
//    on the node context itself, which is why ADK's own SingleTurnTool
//    can RunNode from a tool body — but a TASK-mode LlmAgent never
//    passes it down: RunLLMAgentAsNode (agent/llmagent/
//    llm_agent_wrapper.go) rebuilds the invocation context via
//    icontext.NewInvocationContext, which carries no SubScheduler, so
//    agent.NewToolContext falls back to a fresh commonContext with a
//    nil sub-scheduler. RunNode from a planner tool therefore fails
//    with ErrInvalidRunNodeContext (pinned by
//    TestRunNodeUnavailableFromToolContext). ADK's sanctioned Task
//    delegation loop (TaskAgentTool + dispatchTaskFC) exists only in
//    the chat wrapper, so it is unavailable to a Task-mode planner too.
//
//    What remains is the agenttool pattern: run the specialist under a
//    private runner + in-memory session inside the tool body. Stock
//    tool/agenttool cannot be used directly because its runner would
//    reject a Task-mode specialist as root ("root agent ... must be a
//    chat LlmAgent"), so the dispatcher wraps each specialist in a
//    single-node workflowagent (DynamicNode body -> RunNode ->
//    AgentNode), the same sanctioned dynamic-invocation idiom pkg/graph
//    uses.
//
//    Known gap (documented, not fixed, in v0.1): the sub-runner's
//    events do not stream past the OUTER runner's event consumer, so
//    a specialist's own model calls are invisible to the workload
//    meter in cmd/mast. The planner's OWN model calls are metered
//    like any other agent's (TestPlannerModelCallsAreMetered). Closing
//    the gap is the pre-call-gating / model-wrapper follow-on from
//    docs/orchestration-design.md "Budget substrate" — a metering
//    model.LLM wrapper composes here with zero ADK changes.
//
// 2. request_operator_input mechanism — long-running tool pause, NOT
//    a workflow RequestInput interrupt.
//
//    session.RequestInput (workflow.NewRequestInputEvent +
//    ErrNodeInterrupted, the pkg/graph gate shape with a
//    ResponseSchema and a caller-chosen InterruptID) is a workflow
//    NODE primitive: it needs the node body's emit function and the
//    scheduler's interrupt bookkeeping, neither of which a
//    functiontool context has. From a tool body ADK v2.1.0 allows
//    exactly two HITL shapes: (a) ctx.RequestConfirmation(hint,
//    payload) — boolean approve/reject with a free-form payload, no
//    response schema; or (b) declaring the tool IsLongRunning and
//    returning a nil result, which stamps the pending function-call
//    ID into Event.LongRunningToolIDs and ends the round without a
//    FunctionResponse. This scaffold uses (b): it pauses the planner
//    durably (the wrapper node parks via WithRaiseOnWait, and
//    workflow.ReconstructRunState treats any LongRunningToolIDs entry
//    as an interrupt), and the operator resumes with a
//    FunctionResponse whose ID equals the pending function-call ID.
//    The differences from the design's RequestInputEvent contract are
//    findings, not bugs: the interrupt ID is the model-generated
//    function-call ID (not a deterministic "approve-<name>"), and the
//    schema argument is carried in the event for the operator surface
//    but NOT enforced by ADK — response validation against it would
//    be mast-side at resume. The ResumedInput-first re-entry contract
//    from docs/spike-findings.md does not apply at tool level: the
//    tool is never re-entered on resume; the resume FunctionResponse
//    lands in session history and the planner's next model round
//    continues from it.

import (
	"fmt"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"
)

// NewRoot constructs the planner and wraps it as a runnable root
// agent: a single-node workflow (DynamicNode -> RunNode -> AgentNode)
// around the Task-mode planner, because ADK's runner rejects
// non-Chat LlmAgent roots. The wrapper:
//
//   - forwards every planner event to the runner's event stream (so
//     the workload meter and event log see planner turns), and
//   - propagates a request_operator_input pause as a durable workflow
//     interrupt (WithRaiseOnWait -> ErrNodeInterrupted -> node parks).
//
// The body passes nil node input on every activation: the planner
// reads the work item from session history (the runner appends the
// inject turn before dispatch), which keeps fresh runs and
// post-interrupt resume re-runs identical — on resume the planner
// simply continues from history, where the operator's
// FunctionResponse now answers the pending call.
func NewRoot(cfg Config) (adkagent.Agent, error) {
	p, err := New(cfg)
	if err != nil {
		return nil, err
	}
	node, err := workflow.NewAgentNode(p, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("planner: wrap planner agent: %w", err)
	}
	body := workflow.NewDynamicNode[any, any]("run_planner",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (any, error) {
			return workflow.RunNode[any](ctx, node, nil, workflow.WithRaiseOnWait())
		}, workflow.NodeConfig{})
	return workflowagent.New(workflowagent.Config{
		Name:        cfg.Name + "_planner_root",
		Description: cfg.Description,
		Edges:       workflow.Chain(workflow.Start, body),
		// Register the planner so the runner can resolve authorship of
		// its emitted events.
		SubAgents: []adkagent.Agent{p},
	})
}

// newDispatcher wraps one built specialist as a self-contained
// runnable agent for invoke_specialist's private sub-runner. The
// wrapper exists because Task-mode agents can be neither runner roots
// nor static graph nodes driven directly — the sanctioned shape is a
// DynamicNode body invoking the specialist's AgentNode via RunNode
// (adk/v2 examples/workflow/dynamic/llm; same idiom as pkg/graph).
func newDispatcher(name string, sp adkagent.Agent) (adkagent.Agent, error) {
	if sp == nil {
		return nil, fmt.Errorf("specialist %q is nil", name)
	}
	spNode, err := workflow.NewAgentNode(sp, workflow.NodeConfig{})
	if err != nil {
		return nil, err
	}
	dyn := workflow.NewDynamicNode[any, any]("dispatch",
		func(ctx adkagent.Context, input any, _ func(*session.Event) error) (any, error) {
			return workflow.RunNode[any](ctx, spNode, input)
		}, workflow.NodeConfig{})
	return workflowagent.New(workflowagent.Config{
		Name:        name + "_dispatch",
		Description: "invoke_specialist dispatch wrapper for " + name,
		Edges:       workflow.Chain(workflow.Start, dyn),
		SubAgents:   []adkagent.Agent{sp},
	})
}

// invokeSpecialistArgs is the pinned v0.1 schema of invoke_specialist.
type invokeSpecialistArgs struct {
	// Name is the specialist to run, from the bundle roster.
	Name string `json:"name"`
	// Input is the self-contained work item handed to the specialist.
	Input string `json:"input"`
}

func newInvokeSpecialistTool(roster []string, dispatchers map[string]adkagent.Agent) (tool.Tool, error) {
	available := "(none declared)"
	if len(roster) > 0 {
		available = fmt.Sprintf("%v", roster)
	}
	return functiontool.New(functiontool.Config{
		Name: ToolInvokeSpecialist,
		Description: "Run one specialist from the workload roster as an isolated agent and " +
			"return its structured result. Available specialists: " + available + ".",
	}, func(ctx adkagent.Context, args invokeSpecialistArgs) (map[string]any, error) {
		d, ok := dispatchers[args.Name]
		if !ok {
			return map[string]any{
				"status":     "error",
				"error":      "unknown_specialist",
				"specialist": args.Name,
				"available":  roster,
			}, nil
		}
		// TODO(v0.2 sub-runner debt): this inner runner carries neither
		// the outer budget meter nor the recorded-effect outbox plugin
		// (pkg/effects). The outbox's containment still holds one level
		// up — invoke_specialist is ClassSpawning, refused outright in
		// ambiguous-effect mode — but tool calls made in here leave no
		// durable intent records (in-memory session). Fold into the
		// planner-budget-bypass fix.
		r, err := runner.New(runner.Config{
			AppName:           "planner_dispatch",
			Agent:             d,
			SessionService:    session.InMemoryService(),
			AutoCreateSession: true,
		})
		if err != nil {
			return nil, fmt.Errorf("invoke_specialist %q: construct runner: %w", args.Name, err)
		}

		var (
			output      any
			lastText    string
			totalTokens int64
			modelCalls  int
		)
		msg := genai.NewContentFromText(args.Input, genai.RoleUser)
		for ev, err := range r.Run(ctx, ctx.UserID(), "invoke-"+args.Name, msg, adkagent.RunConfig{}) {
			if err != nil {
				return nil, fmt.Errorf("invoke_specialist %q: %w", args.Name, err)
			}
			if ev == nil {
				continue
			}
			if ev.UsageMetadata != nil {
				modelCalls++
				totalTokens += int64(ev.UsageMetadata.TotalTokenCount)
			}
			if ev.Output != nil {
				output = ev.Output
			}
			if ev.Content != nil {
				for _, part := range ev.Content.Parts {
					if part != nil && part.Text != "" {
						lastText = part.Text
					}
				}
			}
		}
		if output == nil && lastText != "" {
			output = lastText
		}
		return map[string]any{
			"specialist": args.Name,
			"result":     output,
			// Sub-invocation usage is surfaced here because it is
			// invisible to the outer runner's meter (see the mechanism
			// note at the top of this file).
			"sub_model_calls":  modelCalls,
			"sub_total_tokens": totalTokens,
		}, nil
	})
}

// requestOperatorInputArgs is the pinned v0.1 schema of
// request_operator_input.
type requestOperatorInputArgs struct {
	// Message states exactly what decision or input is needed.
	Message string `json:"message"`
	// Schema optionally describes the expected response shape (JSON
	// Schema, as a plain object). Recorded on the pending function
	// call for the operator surface; NOT enforced by ADK at resume —
	// see the mechanism note at the top of this file.
	Schema map[string]any `json:"schema,omitempty"`
}

func newRequestOperatorInputTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: ToolRequestOperatorInput,
		Description: "Escalate to a human operator and pause until they respond. " +
			"State exactly what decision or input you need in message; optionally " +
			"describe the expected response shape in schema.",
		IsLongRunning: true,
	}, func(ctx adkagent.Context, args requestOperatorInputArgs) (map[string]any, error) {
		// Returning a nil result from a long-running tool is the
		// pause: no FunctionResponse is synthesized, the pending
		// call's ID lands in Event.LongRunningToolIDs, and the Task
		// wrapper ends the round. The operator resumes by submitting
		// a FunctionResponse whose ID equals that pending call ID
		// (POST /resume with interrupt_id = the function-call ID).
		return nil, nil
	})
}
