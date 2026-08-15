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
	"log/slog"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"

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

// ExecuteRoute is the route key a diagnoser's node emits when its
// finding carries an approved, executable change (v0.4 W7.0). It is
// namespaced so it cannot collide with a specialist name, which is what
// every other route key in this graph is.
const ExecuteRoute = "mast:execute_change"

// reportNodeName is the graph's single terminal node once the
// diagnoser→executor handoff is wired. See Build.
const reportNodeName = "report"

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

	// Capability is what this specialist may do to the world. Build
	// reads it to find the roster's change executor — the node an
	// approved, executable finding is routed to.
	Capability specialists.Capability
}

// ChangeSetLookup answers, for one specialist, "did the finding it just
// produced carry an executable change?" — returning a rendered
// description of the approved calls.
//
// It is injected rather than read directly because the record lives in
// pkg/approval's state key, and pkg/approval's own tests import
// pkg/graph; importing back the other way is a cycle. internal/compose
// imports both and is the one place that can wire them together.
//
// It must read durable session state, not a value carried over from
// earlier in the turn: under graph dispatch a confirmation resume
// re-enters the workflow at START and re-runs the upstream nodes, so
// nothing computed on the first pass is still in hand
// (docs/spike-findings.md).
type ChangeSetLookup func(ctx adkagent.Context, specialist string) (string, bool)

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

	// ApprovedChangeSet enables the diagnoser→executor handoff (v0.4
	// W7.0). Nil leaves the graph in its v0.3 shape, where every
	// specialist node is terminal and a finding's remediation is
	// something an operator carries out by hand.
	ApprovedChangeSet ChangeSetLookup

	// Logger records the decisions Build makes that a roster author
	// would otherwise have to infer from behavior — chiefly a roster
	// whose shape leaves the handoff off. Optional.
	Logger *slog.Logger
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
			prior, _ := recordedRoute(ctx)
			key := routeKey(input, known, prior)
			ev := session.NewEvent(ctx, ctx.InvocationID())
			ev.Routes = []string{key}
			ev.Actions.StateDelta = map[string]any{routeStateKey: key}
			if err := emit(ev); err != nil {
				return nil, err
			}
			return nil, nil
		}, workflow.NodeConfig{})

	edges := workflow.Chain(workflow.Start, classifyNode, routeNode)
	subAgents := []adkagent.Agent{cfg.Classifier}

	executorName := changeExecutor(cfg)
	// The handoff turns every specialist node from a terminal into a
	// source, so the graph needs somewhere for a run to end. report is
	// a pass-through: whatever reached it — a finding with nothing to
	// execute, or the executor's account of what it did — is the
	// workflow's output, exactly as the specialist node's own output
	// was before. Making it the *single* terminal is the point:
	// ErrMultipleTerminalOutputs fires when two terminals produce
	// output in one run, and a diagnoser handing off to an executor is
	// precisely that shape.
	var reportNode workflow.Node
	if executorName != "" {
		reportNode = workflow.NewFunctionNode[any, any](reportNodeName,
			func(_ adkagent.Context, in any) (any, error) { return in, nil },
			workflow.NodeConfig{})
	}
	runNodes := make(map[string]workflow.Node, len(cfg.Specialists))

	for _, name := range rosterOrder(cfg.Bundle, cfg.Specialists) {
		sp := cfg.Specialists[name]
		spNode, err := workflow.NewAgentNode(sp.Agent, nodeConfig(sp.Budget))
		if err != nil {
			return nil, fmt.Errorf("graph: wrap specialist %q: %w", name, err)
		}
		name := name
		interruptID := "approve-" + name
		stateKey := "triage:" + name
		// A change executor does not hand off to itself, and it is the
		// node that consumes the handoff rather than producing one.
		isExecutor := executorName != "" && name == executorName
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
					// The operator has answered. This is the moment the
					// structural predicate is decidable: an executable
					// change was proposed AND the verdict approved it.
					// Both halves are read here rather than carried from
					// the first pass, which no longer exists.
					out := any(map[string]any{
						"triage":   triage,
						"approval": verdict,
					})
					if isExecutor {
						return out, nil
					}
					return routeChange(ctx, emit, cfg.ApprovedChangeSet, name, verdictApproved(verdict), out)
				}

				// First pass: run the specialist on the original incident
				// envelope (the route node forwards only the route key).
				result, err := workflow.RunNode[any](ctx, spNode, incidentText(ctx))
				if err != nil {
					return nil, err
				}
				if !cfg.Bundle.HITL.RequireApproval {
					// No approval step to wait for, so the workload has
					// already said yes to the roster acting on its own
					// findings. Each of the executor's calls still meets
					// the write gate, which is where on_mutation applies.
					if isExecutor {
						return result, nil
					}
					return routeChange(ctx, emit, cfg.ApprovedChangeSet, name, true, result)
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
		runNodes[name] = runNode
	}

	if executorName != "" {
		execNode := runNodes[executorName]
		for _, name := range rosterOrder(cfg.Bundle, cfg.Specialists) {
			if name == executorName {
				continue
			}
			edges = append(edges,
				workflow.Edge{From: runNodes[name], To: execNode, Route: workflow.StringRoute(ExecuteRoute)},
				// Every other outcome — no change proposed, a change
				// the operator declined, a specialist that never had a
				// change set to begin with — ends the run with the
				// finding as the answer.
				workflow.Edge{From: runNodes[name], To: reportNode, Route: workflow.Default},
			)
		}
		edges = append(edges, workflow.Edge{From: execNode, To: reportNode})
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

// approvalPreamble introduces the approved calls in the event that
// carries them to the change executor.
//
// A session event rather than the node's input, which is the obvious
// carrier and does not work: a Task-mode specialist reached through
// workflow.RunNode assembles its prompt from the session's user content
// and the contextual events on its branch, not from the node input's
// UserContent — verified in TestApprovedChangeReachesTheExecutor, which
// fails if this is passed as input instead. A durable state key does
// not work either, for a different reason: a StateDelta emitted
// mid-turn lands when its event is committed, after the executor node
// has already run.
//
// So the approval is stated in the transcript, which is also the right
// place for it: what the executor is acting on is a fact about this
// incident, and an operator reading the session back sees the approval
// between the finding and the calls that followed it.
const approvalPreamble = `An operator has APPROVED the following calls, exactly as written. Make them, in this
order, with these arguments — do not re-derive them, do not adjust them, and do not
add any others. If one of them cannot be made as written, stop and report why.

`

// routeKey normalizes the classifier's free-text reply into a route
// key: a reason naming a specialist (case-insensitively, trailing period
// tolerated) becomes that specialist's canonical name, and anything else
// emits as-is so the Default edge picks it up.
//
// prior — the route this session last dispatched on, "" if none — is the
// resume path. A resume re-enters the graph at START, so the classifier
// runs again, on a turn whose content is an operator's answer rather
// than an incident. It has nothing to classify and says so, and taking
// that at face value would route the answer to a DIFFERENT specialist
// than the one that asked the question. The first pass's route is
// durable precisely so the second pass does not have to re-derive it
// (docs/spike-findings.md, W2.1's asymmetry).
//
// Only an UNRECOGNIZED reply falls back this way. A classifier that
// names a specialist is believed, and a first pass with nothing recorded
// still reaches the Default edge, so an incident whose reason has no
// specialist goes to _fallback exactly as before.
func routeKey(input any, known map[string]string, prior string) string {
	key := strings.TrimSpace(fmt.Sprint(input))
	if canonical, recognized := known[strings.ToLower(strings.TrimRight(key, "."))]; recognized {
		return canonical
	}
	if strings.TrimSpace(prior) != "" {
		return prior
	}
	return key
}

// routeStateKey holds the route this session last dispatched on, so a
// resume turn lands on the specialist that parked rather than on
// whichever one a re-run classifier picks from an operator's answer.
const routeStateKey = "mast_route"

// recordedRoute reads the route back. An unreadable or empty record is
// "none": the caller then uses the classifier's own reply, which is the
// pre-existing behavior.
func recordedRoute(ctx adkagent.Context) (string, bool) {
	state := ctx.State()
	if state == nil {
		return "", false
	}
	v, err := state.Get(routeStateKey)
	if err != nil || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

// changeExecutor names the roster's change executor, or "" when the
// diagnoser→executor handoff is not wired.
//
// Exactly one change executor is the shape docs/specialists-design.md
// describes and the shape gke-triage ships; anything else leaves an
// approved change without an unambiguous destination and turns the
// handoff off. There is deliberately no error return: no roster shape
// is refused here, because none of them is wrong — they are just not
// all wirable. The unwired cases are either silent by design (no
// lookup, no executor: nothing to hand over) or logged (see below).
func changeExecutor(cfg Config) string {
	if cfg.ApprovedChangeSet == nil {
		return ""
	}
	var found []string
	for _, name := range rosterOrder(cfg.Bundle, cfg.Specialists) {
		if cfg.Specialists[name].Capability == specialists.CapabilityChangeExecutor {
			found = append(found, name)
		}
	}
	switch len(found) {
	case 0:
		return ""
	case 1:
		return found[0]
	}
	// Several executors, so an approved change has no unambiguous
	// destination — but this is not a reason to refuse the roster.
	// Multiple write-capable specialists were legal before W7.0 and are
	// legal still (the capability split cares that a writer declares
	// itself, not that there is only one), and a roster that never
	// proposes a change is unaffected by any of this. What it does mean
	// is that a change set this roster produces will reach an operator
	// as a finding and stop there, so say so loudly rather than leaving
	// it to be discovered as a proposal that never executed.
	if cfg.Logger != nil {
		cfg.Logger.Error("the diagnoser-to-executor handoff is OFF for this roster: it declares more than one change executor, so an approved change has no unambiguous destination; give the roster exactly one specialist with capability: change_executor to turn it on",
			"workload", cfg.Bundle.Name, "change_executors", strings.Join(found, ", "))
	}
	return ""
}

// routeChange is the structural predicate: a finding routes to the
// change executor when it proposed an executable change AND the
// operator approved it. Anything less falls to the Default edge and the
// finding is the run's answer.
//
// The predicate is structural — read off durable state and a recorded
// verdict — rather than a model's judgement about whether remediation
// is warranted. That is the whole point of W7.0: what executes is what
// an operator approved, decided by the graph, not re-litigated by a
// language model that has already been told the answer it wants.
func routeChange(ctx adkagent.Context, emit func(*session.Event) error, lookup ChangeSetLookup, specialist string, approved bool, finding any) (any, error) {
	if lookup == nil {
		return finding, nil
	}
	changes, proposed := lookup(ctx, specialist)
	if !proposed || !approved {
		return finding, nil
	}
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Routes = []string{ExecuteRoute}
	// RoleModel, and NOT RoleUser, for a reason that has nothing to do
	// with how the text reads. ADK authors an event "user" when its
	// content role is user (agent.getAuthorForEvent), and its
	// confirmation resume — the processor that re-dispatches a call an
	// operator approved at the write gate — scans backwards for the
	// most recent user-authored event and gives up if that event has no
	// FunctionResponse in it (llminternal.RequestConfirmationRequestProcessor).
	// A user-authored announcement emitted on the resume pass therefore
	// lands between the operator's confirmation and the executor, and
	// the approved call is never made: the run ends idle, having
	// changed nothing, with every log line reading like success. This
	// event is mast speaking, not the operator, so model role is also
	// the honest one — the executor reads it as
	// "[<workload>_graph] said: ..." (ConvertForeignEvent).
	ev.Content = genai.NewContentFromText(approvalPreamble+changes, genai.RoleModel)
	if err := emit(ev); err != nil {
		return nil, err
	}
	return finding, nil
}

// verdictApproved reads the operator's answer to the approval prompt.
//
// Anything it cannot read as an explicit yes is a no. A verdict that
// arrived in an unexpected shape is not consent, and the cost of the
// two mistakes is not symmetric: refusing to execute leaves an operator
// with a finding, executing without consent leaves them with a changed
// cluster.
func verdictApproved(verdict any) bool {
	switch v := verdict.(type) {
	case bool:
		return v
	case map[string]any:
		approved, _ := v["approved"].(bool)
		return approved
	}
	return false
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
