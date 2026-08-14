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

// Fan-out dispatch (docs/v0.3-plan.md W3): N analysts run concurrently
// over the same incident, one synthesis specialist merges what they
// return into a single report.
//
//	START → fan_out (DynamicNode) → run__synthesis
//	            └─ parallelagent → branch_<name> (workflowagent)
//	                                   └─ run_<name> → <name>
//
// This is the narrow shape, not the general run_shape_fan_out_fan_in:
// the branch set is the roster, fixed at construction, and remediation
// stays sequential and post-synthesis. Nothing in a branch can mutate —
// see BuildFanout's construction check and the reason for it below.
//
// # Why parallelagent and not ParallelWorker
//
// workflow.ParallelWorker is the obvious primitive and it is the wrong
// one for a roster of agents. It suppresses every event a branch emits
// that is not an output event (its own doc says so; runWrappedOnce
// keeps only extractOutput hits). An LLM agent's working memory is the
// session event list — internal/llminternal/contents_processor rebuilds
// req.Contents from ctx.Session().Events() on every model call, and the
// only thing that puts an event there is the RUNNER appending what was
// yielded up to it. Suppress the yield and the agent cannot see its own
// tool results: call #2 gets the same prompt as call #1, so a
// tool-using analyst loops until something cancels it. That is not a
// corner case, it is every analyst that reads a cluster.
// TestBranchAgentSeesItsToolResults pins the behaviour and
// TestFanoutSubstrate records the mechanism.
//
// parallelagent funnels each sub-agent's events upward instead, and
// blocks the sub-agent until the parent has appended the event
// ("Signal sub-agent that event processing (including session append)
// is complete"). Branch isolation is preserved by branch tagging rather
// than by suppression: each sub-agent runs under branch
// "<fan>.<branch_name>" and the history filter admits only events on
// its own branch prefix, so analysts still cannot read each other.
//
// Two composition facts make it fit:
//
//   - A Task specialist cannot be a parallelagent sub-agent directly.
//     parallelagent calls agent.Run, and Task-mode completion lives on
//     the AgentNode/RunNode path, so finish_task returns and the agent
//     keeps going. Each branch is therefore its own single-node
//     workflowagent whose DynamicNode calls workflow.RunNode — legal
//     because DynamicNode.Run installs its own sub-scheduler.
//   - parallelagent runs every sub-agent at once. fanout.max_concurrency
//     is enforced by mast, with a per-activation semaphore the branches
//     acquire before their model call (see branchSemaphores).
//
// Because branch events now reach the runner, two things that were
// false under ParallelWorker are true here: per-specialist budget
// scopes bite inside a branch (the meter buckets by event author over
// the runner's stream), and a branch's tool calls are in the event log
// where crash recovery can see them.
//
// The construction-time refusal of mutating analysts (W3.3) stays, for
// the reason that survives that change: every branch runs BEFORE the
// one approval gate this shape has, which sits after synthesis. A
// mutating analyst is a mutation no operator was offered the chance to
// refuse — concurrently, N of them.
package graph

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/agent/workflowagents/parallelagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// SynthesisName is the specialist that merges the analysts' findings
// into one report. Required in fan-out dispatch, and named by
// convention for the same reason FallbackName is: the shape needs a
// distinguished member of the roster, and a reserved name keeps that
// out of the bundle schema.
const SynthesisName = "_synthesis"

// DefaultMaxConcurrency bounds analyst branches when the bundle names
// no fanout.max_concurrency. Four is a floor on usefulness rather than
// a tuned number: enough that the shape is visibly concurrent, low
// enough that a roster of a dozen analysts does not open a dozen
// simultaneous provider connections by default.
const DefaultMaxConcurrency = 4

// SynthesisInterruptID is the change-safety-gate interrupt the merged
// report parks on when the bundle sets hitl.require_approval. One gate
// per run, after synthesis: an analyst branch never parks. A branch is
// a nested workflowagent with its own scheduler, so an interrupt raised
// inside one is that scheduler's to resolve and the outer graph has no
// pause to record — which is why the branch tool check refuses
// request_operator_input outright rather than trusting an analyst not
// to call it.
const SynthesisInterruptID = "approve-" + SynthesisName

// Analyst is one fan-out branch: a Task specialist plus the two things
// BuildFanout has to check about it that the built agent no longer
// carries — its declared budget and its tool allowlist.
type Analyst struct {
	// Name is the specialist's name, used for the branch label and for
	// attributing findings in the synthesis input.
	Name string

	// Agent is the built specialist.
	Agent adkagent.Agent

	// Budget is the specialist's declared budget block; only
	// MaxWallclockSeconds is consumed here (→ NodeConfig.Timeout), the
	// same mapping graph dispatch makes. max_turns and max_cost_usd
	// reach the session meter as scopes instead (see nodeConfig), and
	// they do bite in a branch: internal/compose.MeterScopes buckets by
	// session.Event.Author over the runner's stream, and parallelagent
	// puts branch events on that stream. This was the one thing
	// ParallelWorker cost that was not visible from reading it — see
	// the package comment.
	Budget specialists.Budget

	// Tools is the specialist's declared allowlist, checked at
	// construction against the mutation predicate.
	Tools specialists.ToolAllowlist
}

// FanoutConfig describes how to assemble the fan-out shape.
type FanoutConfig struct {
	// Bundle is the workload definition; used for naming, HITL policy
	// and fanout.max_concurrency.
	Bundle workload.Bundle

	// Analysts are the concurrent branches, in the order their findings
	// are presented to synthesis. Must be non-empty.
	Analysts []Analyst

	// Synthesis is the specialist that merges the findings. Required.
	Synthesis Specialist

	// Mutating classifies a tool name. Nil means
	// effects.NewPredicate(nil) — mast's default-deny-unknown stance,
	// under which every MCP tool is mutating until the workload's
	// tool_catalog says otherwise.
	Mutating effects.Predicate
}

// BuildFanout assembles the fan-out graph and wraps it as a runnable
// root agent.
//
// It refuses to build a roster whose analysts can mutate. That check is
// deliberately strict about what "can mutate" means: an allowlist that
// does not enumerate its tools is not a narrower grant but a wider one
// (pkg/specialists.filterToolsets passes the whole toolset through when
// a spec names no MCP servers, and the whole server through when it
// names a server with no tools), so an unenumerated analyst is refused
// alongside an explicitly-mutating one.
func BuildFanout(cfg FanoutConfig) (adkagent.Agent, error) {
	if len(cfg.Analysts) == 0 {
		return nil, fmt.Errorf("graph: workload %q has no fan-out analysts; fanout dispatch needs at least one Task specialist besides %q", cfg.Bundle.Name, SynthesisName)
	}
	// The branch check runs before the missing-merger check, so a roster
	// that is BOTH mutating and mergerless reports the mutation. A
	// missing `_synthesis` is a file somebody has not written yet; a
	// mutating analyst is a roster that can never fan out, and that is
	// what an operator who just typed --dispatch=fanout at a remediation
	// bundle needs to be told.
	pred := cfg.Mutating
	if pred == nil {
		pred = effects.NewPredicate(nil)
	}
	if err := checkBranchTools(cfg.Bundle, cfg.Analysts, pred); err != nil {
		return nil, err
	}
	if cfg.Synthesis.Agent == nil {
		return nil, fmt.Errorf("graph: workload %q has no %q specialist; fanout dispatch requires one to merge the analysts' findings", cfg.Bundle.Name, SynthesisName)
	}

	order := make([]string, len(cfg.Analysts))
	sems := &branchSemaphores{limit: maxConcurrency(cfg.Bundle)}
	branches := make([]adkagent.Agent, 0, len(cfg.Analysts))
	for i, a := range cfg.Analysts {
		order[i] = a.Name
		branch, err := branchAgent(a, sems)
		if err != nil {
			return nil, err
		}
		branches = append(branches, branch)
	}

	fan, err := parallelagent.New(parallelagent.Config{AgentConfig: adkagent.Config{
		Name:        cfg.Bundle.Name + "_fan",
		Description: "runs the analyst roster concurrently over one incident",
		SubAgents:   branches,
	}})
	if err != nil {
		return nil, fmt.Errorf("graph: build fan-out agent: %w", err)
	}
	fanNode, err := workflow.NewAgentNode(fan, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("graph: wrap fan-out agent: %w", err)
	}

	// fan_out runs the whole roster and reads the findings back out of
	// the session; it is one node rather than a parallel stage plus a
	// collector so that the roster has exactly one predecessor edge into
	// synthesis.
	//
	// There is deliberately no resume guard here. A resumed response is
	// routed to the asker's successors, and nodes upstream of the asker
	// are not re-entered — in-process or after a process restart, since
	// run state is reconstructed from the event log and parallelagent
	// puts the branches' events in it. TestFanoutResumeKeepsTheFindings
	// pins that the analysts do not re-run on an approval turn; an
	// earlier ctx.ResumedInput guard here was measured to never fire.
	fanOutNode := workflow.NewDynamicNode[any, *Findings]("fan_out",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (*Findings, error) {
			defer sems.forget(ctx.InvocationID())
			if _, err := workflow.RunNode[any](ctx, fanNode, incidentText(ctx)); err != nil {
				return nil, err
			}
			return collectFindings(ctx, order), nil
		}, workflow.NodeConfig{})

	synthesisNode, err := workflow.NewAgentNode(cfg.Synthesis.Agent, nodeConfig(cfg.Synthesis.Budget))
	if err != nil {
		return nil, fmt.Errorf("graph: wrap %q: %w", SynthesisName, err)
	}
	runSynthesis := workflow.NewDynamicNode[*Findings, any]("run_"+SynthesisName,
		func(ctx adkagent.Context, findings *Findings, emit func(*session.Event) error) (any, error) {
			return synthesize(ctx, cfg.Bundle, synthesisNode, findings, emit)
		}, workflow.NodeConfig{})

	return workflowagent.New(workflowagent.Config{
		Name:        cfg.Bundle.Name + "_fanout",
		Description: cfg.Bundle.Description,
		Edges:       workflow.Chain(workflow.Start, fanOutNode, runSynthesis),
		// Register wrapped agents so the runner can resolve event
		// authorship for their emitted events. The analysts are the
		// fan-out agent's own sub-agents, not this one's — ADK refuses
		// an agent tree in which a node has two parents.
		SubAgents: []adkagent.Agent{fan, cfg.Synthesis.Agent},
	})
}

// branchAgent wraps one analyst as a parallelagent sub-agent: a
// single-node workflowagent whose DynamicNode reaches the specialist
// through workflow.RunNode.
//
// The wrapper is not ceremony. parallelagent runs a sub-agent with
// agent.Run, and a Task specialist run that way never completes —
// Task-mode completion is implemented on the AgentNode/RunNode path, so
// finish_task returns a function response and the agent takes another
// turn. ADK refuses a Task agent as a static graph node for the same
// reason ("use a chat coordinator with task sub-agents, or dispatch
// dynamically via RunNode from a function node"), which is exactly the
// shape below.
func branchAgent(a Analyst, sems *branchSemaphores) (adkagent.Agent, error) {
	node, err := workflow.NewAgentNode(a.Agent, nodeConfig(a.Budget))
	if err != nil {
		return nil, fmt.Errorf("graph: wrap analyst %q: %w", a.Name, err)
	}
	// WithUseAsOutput: exactly one output event per branch. Without it
	// the child's output event and the dynamic node's own terminal
	// event both carry an Output and the finding is reported twice.
	run := workflow.NewDynamicNode[any, any]("run_"+a.Name,
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (any, error) {
			release, ok := sems.acquire(ctx)
			if !ok {
				return nil, ctx.Err()
			}
			defer release()
			return workflow.RunNode[any](ctx, node, incidentText(ctx), workflow.WithUseAsOutput())
		}, workflow.NodeConfig{})
	branch, err := workflowagent.New(workflowagent.Config{
		Name:        BranchPrefix + a.Name,
		Description: a.Agent.Description(),
		Edges:       workflow.Chain(workflow.Start, run),
		SubAgents:   []adkagent.Agent{a.Agent},
	})
	if err != nil {
		return nil, fmt.Errorf("graph: wrap analyst %q as a branch: %w", a.Name, err)
	}
	return branch, nil
}

// BranchPrefix names the per-analyst wrapper agent. It shows up in the
// branch tag ("<workload>_fan.branch_<analyst>") and therefore in the
// event log, so it is exported for tests and for anyone reading a
// session back.
const BranchPrefix = "branch_"

// branchSemaphores enforces fanout.max_concurrency, which parallelagent
// itself does not have: it starts every sub-agent at once.
//
// The bound is per activation, not per process. A daemon serving two
// incidents concurrently gets max_concurrency analysts on each, which
// is what a bundle author means by the number — a process-wide
// semaphore would silently couple unrelated sessions. Keying on the
// invocation ID is what makes that possible; branches inherit their
// parent's invocation ID (pinned by TestBranchesShareTheInvocation).
type branchSemaphores struct {
	// limit mirrors ADK's convention: <= 0 is unbounded.
	limit int

	mu   sync.Mutex
	byID map[string]chan struct{}
}

// acquire blocks until a slot is free or the context ends. The returned
// release is safe to call exactly once; ok is false only when the
// context ended first.
func (s *branchSemaphores) acquire(ctx adkagent.Context) (release func(), ok bool) {
	if s == nil || s.limit <= 0 {
		return func() {}, true
	}
	s.mu.Lock()
	if s.byID == nil {
		s.byID = map[string]chan struct{}{}
	}
	id := ctx.InvocationID()
	sem, found := s.byID[id]
	if !found {
		sem = make(chan struct{}, s.limit)
		s.byID[id] = sem
	}
	s.mu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

// forget drops an activation's semaphore. Called when the fan-out stage
// is done, so a long-lived daemon does not accumulate one channel per
// incident it has ever handled.
func (s *branchSemaphores) forget(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.byID, id)
	s.mu.Unlock()
}

// maxConcurrency resolves the branch cap. Negative is passed through:
// ADK's own convention reads <= 0 as unbounded, branchSemaphores
// matches it, and a bundle that says -1 means it.
func maxConcurrency(b workload.Bundle) int {
	if b.Fanout.MaxConcurrency != 0 {
		return b.Fanout.MaxConcurrency
	}
	return DefaultMaxConcurrency
}

// checkBranchTools is W3.3: no analyst may reach a mutating tool.
//
// The rule has to cover three cases and only one of them is the obvious
// one. An analyst that names a mutating tool is refused; so is one that
// names an MCP server without enumerating tools, and so is one that
// names no server at all while the workload has a tool catalog —
// because both of those inherit MORE than an explicit list, not less.
// Under mast's default-deny-unknown predicate an un-enumerated grant is
// a grant of mutating tools whether or not any exist today, which also
// means fan-out forces a workload to classify its read-only tools by
// name in tool_catalog.tools. That is the intended cost: the alternative
// is trusting a name.
func checkBranchTools(b workload.Bundle, analysts []Analyst, pred effects.Predicate) error {
	hasCatalog := len(b.ToolCatalog.MCP) > 0
	for _, a := range analysts {
		if hasCatalog && a.Tools.InheritsAllMCP() {
			return fmt.Errorf("graph: fan-out analyst %q declares no tools.mcp allowlist, which grants it the workload's whole tool catalog; a fan-out branch must enumerate the read-only tools it needs, or write `mcp: []` if it needs none (every branch runs before the one approval gate)", a.Name)
		}
		for _, al := range a.Tools.MCP {
			if len(al.Tools) == 0 {
				return fmt.Errorf("graph: fan-out analyst %q allows MCP server %q with no tools: list, which grants it every tool on that server; enumerate the read-only tools it needs", a.Name, al.Server)
			}
			if bad := mutatingNames(al.Tools, pred); len(bad) > 0 {
				return fmt.Errorf("graph: fan-out analyst %q allows mutating tool(s) %s on MCP server %q; a fan-out branch must be read-only (every branch runs before the one approval gate, so a mutation in one is a mutation no operator was offered the chance to refuse). Either drop them or classify them read-only in the workload's tool_catalog.tools", a.Name, strings.Join(bad, ", "), al.Server)
			}
		}
		if bad := mutatingNames(a.Tools.Builtin, pred); len(bad) > 0 {
			return fmt.Errorf("graph: fan-out analyst %q allows mutating built-in tool(s) %s; a fan-out branch must be read-only", a.Name, strings.Join(bad, ", "))
		}
		// request_operator_input is classified read-only — it changes
		// nothing in the world — so the mutation check passes it. A
		// branch still may not have it: it parks the run, and a branch
		// has no way to park. See SynthesisInterruptID.
		for _, n := range a.Tools.Builtin {
			if n == operatorInputTool {
				return fmt.Errorf("graph: fan-out analyst %q allows %q; a fan-out branch cannot pause for an operator (the one gate is on the merged report, after synthesis) — ask for what you need in the finding instead", a.Name, operatorInputTool)
			}
		}
	}
	return nil
}

// operatorInputTool is the built-in that parks a run for a human. Named
// here rather than imported from pkg/effects because what this file
// needs is the pause, not the mutation class.
const operatorInputTool = "request_operator_input"

// mutatingNames returns the sorted subset of names the predicate does
// not classify read-only. Spawning counts: a sub-run started from
// inside a branch takes its own tool calls with it, out of sight.
func mutatingNames(names []string, pred effects.Predicate) []string {
	var bad []string
	for _, n := range names {
		if pred(n) != effects.ClassReadOnly {
			bad = append(bad, n)
		}
	}
	sort.Strings(bad)
	return bad
}

// Finding is one analyst's contribution: whatever its branch returned
// as an Output payload, and nothing else.
type Finding struct {
	Analyst string
	Payload any
}

// Findings is what synthesis is allowed to see (W3.2): one Output
// payload per analyst that produced one, and nothing else. A branch's
// Output payload is its only contribution — an analyst that does work
// and returns nothing has, as far as this graph is concerned, done
// nothing, and Silent names it so that is visible rather than merely
// true.
type Findings struct {
	// Reported are the branches that returned a payload, in roster
	// order.
	Reported []Finding

	// Silent are the names of branches that returned no payload.
	Silent []string
}

// collectFindings reads each analyst's finding out of the session.
//
// The session is the right place to look, and under this shape it is
// the only place: parallelagent yields branch events up to the runner,
// which appends them, so by the time the fan-out stage returns every
// analyst's Output event is in the event list authored by the analyst
// itself. Scanning is also what makes silence detectable without a
// positional aggregate — a branch that emitted no output simply is not
// there.
//
// Only events from the current invocation count. A session can be
// injected with a second incident, and the previous incident's findings
// must not be merged into this one's report.
func collectFindings(ctx adkagent.Context, order []string) *Findings {
	want := make(map[string]bool, len(order))
	for _, name := range order {
		want[name] = true
	}
	latest := make(map[string]any, len(order))
	for ev := range ctx.Session().Events().All() {
		if ev == nil || ev.Output == nil || ev.InvocationID != ctx.InvocationID() {
			continue
		}
		if want[ev.Author] {
			latest[ev.Author] = ev.Output
		}
	}
	return findingsFor(order, latest)
}

// findingsFor splits a name→payload map by the roster, so Reported is
// in roster order and every absent name is Silent.
func findingsFor(order []string, byName map[string]any) *Findings {
	out := &Findings{}
	for _, name := range order {
		payload, ok := byName[name]
		if !ok || payload == nil {
			out.Silent = append(out.Silent, name)
			continue
		}
		out.Reported = append(out.Reported, Finding{Analyst: name, Payload: payload})
	}
	return out
}

// synthesize runs the merge specialist over the reported payloads and,
// under hitl.require_approval, parks the merged report on the one
// change-safety gate this shape has.
//
// The all-silent case short-circuits: no model call, and no gate. A
// gate exists to put a human in front of a decision, and there is no
// decision in an empty report — W1.4 recorded the graph-dispatch
// version of this (an operator prompted to approve `Result: <nil>`) as
// a defect, so fan-out does not reproduce it.
func synthesize(ctx adkagent.Context, b workload.Bundle, node workflow.Node, findings *Findings, emit func(*session.Event) error) (any, error) {
	if findings == nil {
		findings = &Findings{}
	}
	result := map[string]any{
		"analysts": len(findings.Reported) + len(findings.Silent),
		"reported": len(findings.Reported),
		"silent":   findings.Silent,
	}
	if len(findings.Reported) == 0 {
		result["report"] = ""
		return result, nil
	}

	// Resume re-entry is checked before re-running the child, for the
	// reason spelled out in Build: RunNode does not cache dynamic
	// children across a pause turn, so an unguarded call re-invokes the
	// specialist and then trips over the resume turn's orphan
	// FunctionResponse.
	if verdict, ok := ctx.ResumedInput(SynthesisInterruptID); ok {
		report, err := ctx.State().Get(synthesisStateKey)
		if err != nil {
			report = "(synthesis report unavailable: " + err.Error() + ")"
		}
		result["report"] = report
		result["approval"] = verdict
		return result, nil
	}

	report, err := workflow.RunNode[any](ctx, node, SynthesisPrompt(findings))
	if err != nil {
		return nil, err
	}
	result["report"] = report
	if !b.HITL.RequireApproval {
		return result, nil
	}

	stash := session.NewEvent(ctx, ctx.InvocationID())
	stash.Actions.StateDelta = map[string]any{synthesisStateKey: report}
	if err := emit(stash); err != nil {
		return nil, err
	}
	if err := emit(workflow.NewRequestInputEvent(ctx, session.RequestInput{
		InterruptID: SynthesisInterruptID,
		Message:     fmt.Sprintf("Approve merged report from %d of %d analysts? Result: %v", len(findings.Reported), len(findings.Reported)+len(findings.Silent), report),
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
}

const synthesisStateKey = "fanout:" + SynthesisName

// SynthesisPrompt renders the merge specialist's input. It reads
// Findings and nothing else — the payload-only contract in one
// function, so there is a single place to look when asking what
// synthesis can see.
func SynthesisPrompt(f *Findings) string {
	var sb strings.Builder
	sb.WriteString("Merge the analyst findings below into one report.\n")
	sb.WriteString("These payloads are the complete record: anything an analyst did\n")
	sb.WriteString("but did not return is not available to you.\n")
	for _, fi := range f.Reported {
		fmt.Fprintf(&sb, "\n## %s\n%v\n", fi.Analyst, fi.Payload)
	}
	if len(f.Silent) > 0 {
		fmt.Fprintf(&sb, "\nReturned no finding: %s\n", strings.Join(f.Silent, ", "))
	}
	return sb.String()
}
