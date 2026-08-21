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
//    A private runner is a private event stream: nothing the sub-run
//    emits reaches the OUTER runner's consumer, which through v0.4 is
//    where every accounting seam mast has rides (the budget meter, the
//    metrics registry, the watchdog tap in cmd/mast). A specialist
//    dispatched through this door could therefore spend without limit,
//    report no tokens, and loop without being halted (#226). The seam
//    that closes it is Config.SubRunObserver: the tool hands the host
//    every sub-run event, so the host folds them into exactly the
//    consumers the outer stream feeds. Coordinator and graph dispatch
//    never had the gap — both funnel sub-agent events upward.
//
//    Enforcement here is better-shaped than at the outer stream, and
//    the reason is worth keeping: an observer error stops only the
//    SUB-run and hands the planner a labelled partial it can route
//    around, rather than cancelling the session. That is the shape
//    pkg/budget's own package doc calls out as needing a pre-call
//    seam — the tool body turns out to be one.
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

// SubRunObserver is handed every event emitted by the private
// sub-runner an invoke_specialist dispatch runs a specialist on.
//
// sessionID is the OUTER session's ID — the sub-run's own session is an
// in-memory throwaway, and attributing a dispatch to it would file the
// spend under a session no operator can name. The event's Author is the
// specialist's agent name, which is what lets a host's per-specialist
// ceilings bind here the same way they bind on a coordinator's dispatch.
//
// Returning an error stops the sub-run: the specialist's remaining work
// is abandoned and invoke_specialist returns a partial result labelled
// with the reason, so the planner sees a refusal it can route around
// instead of a cancelled session. Observers must therefore return an
// error only when the run genuinely should stop — a metering failure
// that is not a ceiling should be logged and swallowed.
type SubRunObserver interface {
	ObserveSubRun(sessionID string, ev *session.Event) error
}

func newInvokeSpecialistTool(roster []string, dispatchers map[string]adkagent.Agent, obs SubRunObserver) (tool.Tool, error) {
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
		// The inner runner's accounting reaches the host through obs
		// (#226). Still outstanding on the same seam: the recorded-effect
		// outbox plugin (pkg/effects) is not installed here, so tool
		// calls made inside a dispatch leave no durable intent records
		// (the sub-session is in-memory). The outbox's containment holds
		// one level up — invoke_specialist is ClassSpawning, refused
		// outright in ambiguous-effect mode.
		r, err := runner.New(runner.Config{
			AppName:           "planner_dispatch",
			Agent:             d,
			SessionService:    session.InMemoryService(),
			AutoCreateSession: true,
		})
		if err != nil {
			return nil, fmt.Errorf("invoke_specialist %q: construct runner: %w", args.Name, err)
		}

		// A cancellable copy of the tool context, so an observer that
		// stops the sub-run stops it here and not one level up: the
		// outer turn keeps running and the planner gets a result.
		// Deferred cancel also guarantees the sub-run cannot outlive
		// the tool call that started it.
		subCtx, cancelSub := ctx.WithAgentCancel()
		defer cancelSub()

		var (
			output      any
			lastText    string
			totalTokens int64
			modelCalls  int
			halted      error
		)
		msg := genai.NewContentFromText(args.Input, genai.RoleUser)
		for ev, err := range r.Run(subCtx, ctx.UserID(), "invoke-"+args.Name, msg, adkagent.RunConfig{}) {
			if err != nil {
				// A halt cancels the sub-context, so the runner's own
				// error after one is "context canceled" — a symptom of
				// the stop we already have the reason for.
				if halted != nil {
					break
				}
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
			// Observe last: the host sees the event that crossed its
			// ceiling, and the partial below carries whatever the
			// specialist had produced up to it.
			if obs != nil {
				if err := obs.ObserveSubRun(ctx.SessionID(), ev); err != nil {
					halted = err
					cancelSub()
					break
				}
			}
		}
		if output == nil && lastText != "" {
			output = lastText
		}
		res := map[string]any{
			"specialist": args.Name,
			"result":     output,
			// Sub-invocation usage is surfaced to the model as well as
			// to obs: the planner budgets its own plan, and it can only
			// do that if it can see what a dispatch cost.
			"sub_model_calls":  modelCalls,
			"sub_total_tokens": totalTokens,
		}
		if halted != nil {
			// Not an error return: a cap that fires must not look like a
			// broken tool. The planner is told the specialist stopped
			// early and why, and decides what to do about it.
			res["status"] = "halted"
			res["reason"] = halted.Error()
		}
		return res, nil
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
