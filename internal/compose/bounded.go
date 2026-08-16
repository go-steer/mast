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

package compose

import (
	"fmt"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/workflow"

	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// Bounded dispatch (docs/v0.4-plan.md W4.3): one model call, forced
// structured output, no orchestrator, a step count that is a constant.
//
//	START → <the one specialist> (SingleTurn AgentNode)
//
// # Why this is a fourth shape and not a mode on the one-shot path
//
// `mast --task=<class>` (oneshot.go, BuildClassRoot) looks like the
// place for it — it already wraps a non-Chat agent in a one-node
// workflow, which is most of the construction below. It is the wrong
// place, and the list of what it does not have is the argument: no
// bundle, so no `hitl:` policy and no `tool_catalog`; no roster, so no
// specialist file to carry `output_schema:` or `tier:`; no meter
// scopes, since MeterScopes derives them from specs; and no toolsets,
// which matters less for the agent than for the fact that the shape has
// to sit under the same write gate every other shape sits under. Its
// caller cannot be scheduled either — the one-shot path exits after one
// turn, and W4.1's scheduler fires workloads. A bounded cycle that a
// timer wakes is a *workload*, so it composes like one.
//
// # What the refusals buy
//
// The roster is exactly one SingleTurn specialist that declares an
// output_schema, and each of the three ways to miss that is a build
// error naming what was found rather than a shape that quietly costs
// more:
//
//   - Two specialists need something to choose between them, and that
//     chooser is the orchestrator this shape exists to remove. Its
//     cheapest form is still a model call, so the roster that would
//     have needed it is refused instead of silently priced.
//   - A Task specialist runs a tool loop to a finish_task, which is a
//     step count the bundle cannot state — "one call" would become
//     "one call plus however many the model wants".
//   - Without an output_schema there is nothing to force. The reply is
//     prose, and prose needs a reader; the reader is either an
//     orchestrator or a human, and this shape has neither.
//
// The schema is not a nicety here the way it is elsewhere. Under a
// toolless SingleTurn agent ADK sets req.Config.ResponseSchema and
// ResponseMIMEType directly (internal/llminternal's basic processor)
// and validates the reply on the way out, so the contract is enforced
// by the provider and then re-checked — the genuine structured-output
// path, not the finish_task workaround a Task agent gets.

// boundedSpec returns the single specialist a bounded roster is allowed
// to hold, or an error naming what the roster actually contains.
//
// It runs before BuildRoot constructs anything, so a roster that can
// never be bounded is refused without first resolving a tier against a
// provider or opening a client for it.
func boundedSpec(b workload.Bundle, specs []specialists.Spec) (specialists.Spec, error) {
	if b.Planner.Enabled {
		// Every other shape lets the planner win and logs that it did.
		// Bounded cannot: the planner IS an orchestrator, and a bundle
		// asking for both has asked for the shape and for the thing the
		// shape is defined by not having. Guessing which one it meant
		// would silently multiply the cost of a cycle by the length of
		// a plan.
		return specialists.Spec{}, fmt.Errorf("compose: workload %q declares dispatch: bounded and planner.enabled; the planner is an orchestrator and the bounded path is defined by not having one — keep one of the two", b.Name)
	}
	if len(specs) != 1 {
		return specialists.Spec{}, fmt.Errorf("compose: workload %q declares dispatch: bounded but its roster is %d specialists (%s); bounded dispatch takes exactly one, because a second one needs something to choose between them and that chooser is the orchestrator this shape exists to remove",
			b.Name, len(specs), rosterNames(specs))
	}
	s := specs[0]
	if s.Mode != specialists.ModeSingleTurn {
		mode := string(s.Mode)
		if mode == "" {
			// An absent mode: is Task (the default specialists.Build
			// applies), and saying "mode Task" about a file with no
			// mode: line sends the reader looking for a line to change.
			mode = string(specialists.ModeTask) + " (the default for a spec with no mode:)"
		}
		return specialists.Spec{}, fmt.Errorf("compose: workload %q declares dispatch: bounded but its specialist %q is mode %s; bounded dispatch requires mode: SingleTurn, because a Task specialist runs a tool loop to a finish_task and the step count stops being a number the bundle can state",
			b.Name, s.Name, mode)
	}
	if s.OutputSchema == nil {
		return specialists.Spec{}, fmt.Errorf("compose: workload %q declares dispatch: bounded but its specialist %q declares no output_schema:; the one call has to return a contract, because there is no orchestrator downstream to read prose and nothing else in this shape will look at the answer",
			b.Name, s.Name)
	}
	return s, nil
}

// rosterNames renders a roster for an error message, in the order the
// specs arrive (the loader hands them over sorted by name). Stable
// rather than pretty: the operator's next move is to compare this list
// against the one in workload.yaml, and a set that reordered run to run
// would make that a reading exercise.
func rosterNames(specs []specialists.Spec) string {
	if len(specs) == 0 {
		return "none"
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// buildBounded wraps the one specialist as a single-node workflow root.
//
// The wrap is not decoration: ADK's runner refuses an LlmAgent root
// that is not Chat mode (runner.go, "root agent %s must be a chat
// LlmAgent"), and a SingleTurn agent is by definition not one. A
// one-node workflow is the sanctioned way to make a non-Chat agent a
// root — the same idiom BuildClassRoot and pkg/planner.NewRoot use —
// and it costs no model call of its own, which is the property the
// whole shape is about.
func buildBounded(b workload.Bundle, spec specialists.Spec, built adkagent.Agent) (adkagent.Agent, error) {
	if built == nil {
		// Unreachable: boundedSpec ran against the same roster BuildRoot
		// then built. Kept as an error rather than a nil dereference so
		// a future edit that reorders the two finds out here.
		return nil, fmt.Errorf("compose: workload %q: bounded specialist %q was not built", b.Name, spec.Name)
	}
	node, err := workflow.NewAgentNode(built, boundedNodeConfig(spec))
	if err != nil {
		return nil, fmt.Errorf("compose: wrap bounded specialist %q: %w", spec.Name, err)
	}
	return workflowagent.New(workflowagent.Config{
		Name:        b.Name + "_bounded",
		Description: b.Description,
		Edges:       workflow.Chain(workflow.Start, node),
		SubAgents:   []adkagent.Agent{built},
	})
}

// boundedNodeConfig maps the specialist's max_wallclock_seconds onto the
// node timeout, the same mapping pkg/graph makes for a specialist node.
// The other two budget fields are the meter's (MeterScopes), not a
// node's — a node cannot see a token count.
func boundedNodeConfig(spec specialists.Spec) workflow.NodeConfig {
	var cfg workflow.NodeConfig
	if spec.Budget.MaxWallclockSeconds > 0 {
		cfg.Timeout = time.Duration(spec.Budget.MaxWallclockSeconds) * time.Second
	}
	return cfg
}
