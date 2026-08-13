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

// Package graph assembles the workload's triage flow as an explicit
// ADK v2 workflow graph — the LLM-as-router shape from
// docs/workflow-scaffolding-design.md (#7), as sketched in
// docs/triage-demo-plan.md:
//
//	START → classify (SingleTurn AgentNode) → route_by_reason
//	          ├─ StringRoute(<reason>) → run_<reason> (DynamicNode → Task specialist)
//	          └─ Default              → run__fallback
//
// Spike-2 finding (supersedes the spike-1 comment in pkg/router):
// runner.Runner does NOT require the root agent to be a Chat-mode
// LlmAgent. The Chat-mode check applies only when the root IS an
// LlmAgent; a workflowagent-wrapped graph takes the runner's generic
// (non-LlmAgent) root path and works as a root agent directly. See
// adk/v2 runner/runner.go (isLlmAgent branch) and
// examples/workflow/routing/llm, which uses a workflowagent root.
//
// Task-mode specialists cannot be static graph nodes, so each routed
// branch is a DynamicNode whose body invokes the specialist's
// AgentNode via workflow.RunNode — the sanctioned dynamic-invocation
// pattern (see adk/v2 examples/workflow/dynamic/llm).
package graph

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// FallbackName is the specialist that handles reasons the classifier
// can't map to a per-failure-mode specialist. Required in graph mode.
const FallbackName = "_fallback"

// Specialist pairs a built Task-mode agent with the budget bounds
// declared on its Spec, so Build can map per-specialist ceilings onto
// per-node ADK config without re-reading spec files.
type Specialist struct {
	// Agent is the built specialist.
	Agent adkagent.Agent

	// Budget is the specialist's declared budget block. Only
	// MaxWallclockSeconds is consumed here (→ NodeConfig.Timeout);
	// see nodeConfig for where the other fields are enforced.
	Budget specialists.Budget
}

// Config describes how to assemble the workflow-graph dispatch shape
// for a workload.
type Config struct {
	// Bundle is the workload definition; used for naming and roster
	// ordering.
	Bundle workload.Bundle

	// Classifier is the SingleTurn routing agent. Its one-shot output
	// is normalized into a route key by the route node.
	Classifier adkagent.Agent

	// Specialists is the roster of Task-mode specialists indexed by
	// spec name. Must contain FallbackName.
	Specialists map[string]Specialist
}

// nodeConfig maps a specialist's declared budget onto the ADK per-node
// config. MaxWallclockSeconds becomes NodeConfig.Timeout — the
// sanctioned per-node wallclock knob (docs/adk-v2-usage.md, NodeConfig
// resolution 2026-07-25; docs/specialists-design.md open Q #2): the
// scheduler wraps each activation in context.WithTimeout, so the cap
// bounds every activation of the specialist's node, including resume
// re-runs. Zero means no per-node timeout; the node is bounded only by
// the dispatch deadline (workload max_wallclock_seconds in cmd/mast).
//
// The other Budget fields are not node-level knobs: max_turns and
// max_cost_usd are enforced by the session meter, which buckets usage
// per specialist by event author (pkg/budget, "Scopes"). A node cannot
// see them — cost is a property of the event stream, not of an
// activation.
func nodeConfig(b specialists.Budget) workflow.NodeConfig {
	var cfg workflow.NodeConfig
	if b.MaxWallclockSeconds > 0 {
		cfg.Timeout = time.Duration(b.MaxWallclockSeconds) * time.Second
	}
	return cfg
}

// Build assembles the graph and wraps it as a runnable root agent via
// workflowagent.New.
func Build(cfg Config) (adkagent.Agent, error) {
	if cfg.Classifier == nil {
		return nil, fmt.Errorf("graph: Config.Classifier is required")
	}
	if _, ok := cfg.Specialists[FallbackName]; !ok {
		return nil, fmt.Errorf("graph: workload %q has no %q specialist; graph dispatch requires one for the Default route", cfg.Bundle.Name, FallbackName)
	}

	classifyNode, err := workflow.NewAgentNode(cfg.Classifier, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("graph: wrap classifier: %w", err)
	}

	// Normalize the classifier's free-text reply into a route key.
	// Reason keys are matched case-insensitively against the roster;
	// anything unrecognized emits as-is and falls to the Default edge.
	known := make(map[string]string, len(cfg.Specialists))
	for name := range cfg.Specialists {
		known[strings.ToLower(name)] = name
	}
	routeNode := workflow.NewEmittingFunctionNode("route_by_reason",
		func(ctx adkagent.Context, input any, emit func(*session.Event) error) (any, error) {
			key := strings.TrimSpace(fmt.Sprint(input))
			if canonical, ok := known[strings.ToLower(strings.TrimRight(key, "."))]; ok {
				key = canonical
			}
			ev := session.NewEvent(ctx, ctx.InvocationID())
			ev.Routes = []string{key}
			if err := emit(ev); err != nil {
				return nil, err
			}
			return nil, nil
		}, workflow.NodeConfig{})

	edges := workflow.Chain(workflow.Start, classifyNode, routeNode)
	subAgents := []adkagent.Agent{cfg.Classifier}

	for _, name := range rosterOrder(cfg.Bundle, cfg.Specialists) {
		sp := cfg.Specialists[name]
		spNode, err := workflow.NewAgentNode(sp.Agent, nodeConfig(sp.Budget))
		if err != nil {
			return nil, fmt.Errorf("graph: wrap specialist %q: %w", name, err)
		}
		name := name
		interruptID := "approve-" + name
		stateKey := "triage:" + name
		runNode := workflow.NewDynamicNode[any, any]("run_"+name,
			func(ctx adkagent.Context, _ any, emit func(*session.Event) error) (any, error) {
				// Resume re-entry MUST be checked before re-running
				// children. Spike-2 finding: RunNode does NOT return
				// cached child results across the pause turn (dynamic
				// children aren't part of the static graph, so
				// ReconstructRunState doesn't rehydrate their outputs) —
				// an unguarded RunNode re-invokes the specialist LLM on
				// resume, and its request assembly then trips over the
				// resume turn's orphan FunctionResponse ("no function
				// call event found..."). The upstream dynamic/hitl
				// example uses exactly this ResumedInput-first shape.
				if verdict, ok := ctx.ResumedInput(interruptID); ok {
					triage, err := ctx.State().Get(stateKey)
					if err != nil {
						triage = "(triage result unavailable: " + err.Error() + ")"
					}
					return map[string]any{
						"triage":   triage,
						"approval": verdict,
					}, nil
				}

				// First pass: run the specialist on the original incident
				// envelope (the route node forwards only the route key).
				result, err := workflow.RunNode[any](ctx, spNode, incidentText(ctx))
				if err != nil {
					return nil, err
				}
				if !cfg.Bundle.HITL.RequireApproval {
					return result, nil
				}

				// Change-safety-gate (docs/triage-demo-plan.md): stash the
				// triage result in session state (the resume pass can't
				// re-derive it without re-running the specialist), then
				// park the node on a durable RequestInput interrupt.
				// InterruptID is deterministic per specialist rather than
				// UUID-fresh: sessions are per-incident in spike 2, so at
				// most one approval exists per (session, specialist), and
				// a knowable ID lets the operator resume across a process
				// restart without scraping state.
				stash := session.NewEvent(ctx, ctx.InvocationID())
				stash.Actions.StateDelta = map[string]any{stateKey: result}
				if err := emit(stash); err != nil {
					return nil, err
				}
				if err := emit(workflow.NewRequestInputEvent(ctx, session.RequestInput{
					InterruptID: interruptID,
					Message:     fmt.Sprintf("Approve triage result from %s? Result: %v", name, result),
					ResponseSchema: &jsonschema.Schema{
						Type: "object",
						Properties: map[string]*jsonschema.Schema{
							"approved": {Type: "boolean"},
							"note":     {Type: "string"},
						},
						Required: []string{"approved"},
					},
				})); err != nil {
					return nil, err
				}
				return nil, workflow.ErrNodeInterrupted
			}, workflow.NodeConfig{})

		var route workflow.Route = workflow.StringRoute(name)
		if name == FallbackName {
			route = workflow.Default
		}
		edges = append(edges, workflow.Edge{From: routeNode, To: runNode, Route: route})
		subAgents = append(subAgents, sp.Agent)
	}

	return workflowagent.New(workflowagent.Config{
		Name:        cfg.Bundle.Name + "_graph",
		Description: cfg.Bundle.Description,
		Edges:       edges,
		// Register wrapped agents so the runner can resolve event
		// authorship for their emitted events.
		SubAgents: subAgents,
	})
}

// rosterOrder yields specialist names in bundle order, restricted to
// those present in the built map (the classifier is not in this map).
func rosterOrder(b workload.Bundle, built map[string]Specialist) []string {
	out := make([]string, 0, len(built))
	seen := make(map[string]bool, len(built))
	for _, name := range b.Specialists {
		if _, ok := built[name]; ok && !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	for name := range built {
		if !seen[name] {
			out = append(out, name)
		}
	}
	return out
}

func incidentText(ctx adkagent.Context) string {
	uc := ctx.UserContent()
	if uc == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range uc.Parts {
		if p != nil {
			sb.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(sb.String())
}
