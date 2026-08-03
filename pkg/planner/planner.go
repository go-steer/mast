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

// Package planner implements the v0.1 scaffold of the supervisor-body
// planner from docs/orchestration-design.md ("The planner", shape C
// with light D flavor): a Task-mode LlmAgent whose tool vocabulary is
// the workload's execution vocabulary. Each planner turn is one tool
// call; the "graph" is emergent from the sequence of decisions and is
// recorded to the event log turn by turn; finish_task (auto-installed
// by ADK for Task mode) is the canonical exit.
//
// v0.1 scaffold scope (orchestration-design phasing, "v0.1" row —
// "Planner scaffolded — schema for tool vocabulary in place, but
// run_shape_* tools may not all be implemented yet"):
//
//   - invoke_specialist(name, input) is implemented and dispatches to
//     the bundle's built specialist roster (see dispatch.go for the
//     mechanism and the ADK constraints that shaped it).
//   - run_shape_llm_router and run_shape_fan_out_fan_in are declared
//     (their schema is part of the pinned vocabulary contract) but
//     return a structured not_implemented result; the Phase-2
//     "reference-graph library" item wires them.
//   - request_operator_input(message, schema) pauses the planner on a
//     durable long-running-tool interrupt (see dispatch.go for what
//     ADK v2.1.0 does and does not allow from a tool context).
//
// Budget: the planner introduces no budget machinery of its own. Its
// model calls stream past the runner event consumer like any other
// agent's, so the workload meter (pkg/budget, wired in cmd/mast)
// bounds the planner exactly as it bounds specialists. See the
// "sub-invocation metering" note in dispatch.go for the one known gap.
package planner

import (
	"fmt"
	"strings"
	"text/template"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

// Tool names of the v0.1 planner vocabulary. Exported so callers and
// tests can pin the contract without string literals. finish_task is
// not listed: ADK auto-installs it for Task mode.
const (
	ToolInvokeSpecialist     = "invoke_specialist"
	ToolRunShapeLLMRouter    = "run_shape_llm_router"
	ToolRunShapeFanOutFanIn  = "run_shape_fan_out_fan_in"
	ToolRequestOperatorInput = "request_operator_input"
	// ToolPauseSession is the v0.2 plane-A self-pause
	// (docs/durable-execution-design.md, "The v0.2 pause/abort
	// mechanics"). Registered only when Config.PauseRecorder is set.
	ToolPauseSession = "pause_session"
)

// Config describes how to construct a planner for one workload bundle.
type Config struct {
	// Name is the workload name; the planner agent is named
	// "<Name>_planner". Required.
	Name string

	// Description is a human-readable summary, surfaced in operator
	// UIs and logs.
	Description string

	// Model is the LLM driving the planner's turn loop. Required.
	Model model.LLM

	// Instruction overrides the default planner instruction template.
	// Empty renders DefaultInstructionTemplate against the roster —
	// the planner-specific frame from orchestration-design's planner
	// section (resolved open question #7: single template with
	// variables; per-workload override as the escape hatch). The
	// generic pkg/agent DefaultTaskInstruction never applies here: a
	// planner is never a generic task agent, so the template embeds
	// the unattended-task discipline itself.
	Instruction string

	// Specialists is the bundle's built roster, indexed by specialist
	// name — the invoke_specialist targets.
	Specialists map[string]adkagent.Agent

	// Order optionally fixes the roster ordering used in the rendered
	// instruction and tool description (bundle declaration order).
	// Names absent from Specialists are ignored; specialists absent
	// from Order are appended in map-iteration-stable sorted order.
	Order []string

	// PauseRecorder mints the durable pause record + resume token for
	// pause_session (the v0.2 plane-A self-pause). Nil leaves
	// pause_session out of the vocabulary — callers without a durable
	// store (one-shot in-memory sessions) get a coherent tool set
	// rather than a pause that would die with the process.
	PauseRecorder PauseRecorder
}

// New constructs the planner as a Task-mode LlmAgent via
// pkg/agent.NewTaskAgent, with the v0.1 tool vocabulary attached.
//
// The returned agent CANNOT be a runner root directly: ADK v2.1.0's
// runner requires an LlmAgent root to be Chat-mode (runner.go,
// "root agent ... must be a chat LlmAgent"). Use NewRoot for the
// runnable wrapper.
func New(cfg Config) (adkagent.Agent, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("planner: Config.Name is required")
	}
	if cfg.Model == nil {
		return nil, fmt.Errorf("planner: Config.Model is required")
	}

	roster := rosterOrder(cfg)

	dispatchers := make(map[string]adkagent.Agent, len(cfg.Specialists))
	for _, name := range roster {
		d, err := newDispatcher(name, cfg.Specialists[name])
		if err != nil {
			return nil, fmt.Errorf("planner: build dispatcher for specialist %q: %w", name, err)
		}
		dispatchers[name] = d
	}

	tools := []tool.Tool{}
	invoke, err := newInvokeSpecialistTool(roster, dispatchers)
	if err != nil {
		return nil, fmt.Errorf("planner: build %s: %w", ToolInvokeSpecialist, err)
	}
	tools = append(tools, invoke)

	router, err := newNotImplementedShapeTool(ToolRunShapeLLMRouter,
		"Run the LLM-as-router reference shape: a bundle-scoped classifier routes to one of the named handlers.",
		func(ctx adkagent.Context, args runShapeLLMRouterArgs) (map[string]any, error) {
			return NotImplemented(ToolRunShapeLLMRouter), nil
		})
	if err != nil {
		return nil, fmt.Errorf("planner: build %s: %w", ToolRunShapeLLMRouter, err)
	}
	tools = append(tools, router)

	fanOut, err := newNotImplementedShapeTool(ToolRunShapeFanOutFanIn,
		"Run the fan-out-fan-in reference shape: a planner function fans work out to the named workers and a joiner folds the results.",
		func(ctx adkagent.Context, args runShapeFanOutFanInArgs) (map[string]any, error) {
			return NotImplemented(ToolRunShapeFanOutFanIn), nil
		})
	if err != nil {
		return nil, fmt.Errorf("planner: build %s: %w", ToolRunShapeFanOutFanIn, err)
	}
	tools = append(tools, fanOut)

	operator, err := newRequestOperatorInputTool()
	if err != nil {
		return nil, fmt.Errorf("planner: build %s: %w", ToolRequestOperatorInput, err)
	}
	tools = append(tools, operator)

	if cfg.PauseRecorder != nil {
		pauseTool, err := newPauseSessionTool(cfg.PauseRecorder)
		if err != nil {
			return nil, fmt.Errorf("planner: build %s: %w", ToolPauseSession, err)
		}
		tools = append(tools, pauseTool)
	}

	instruction := cfg.Instruction
	if instruction == "" {
		instruction, err = renderInstruction(cfg, roster)
		if err != nil {
			return nil, fmt.Errorf("planner: render instruction: %w", err)
		}
	}

	return mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        cfg.Name + "_planner",
		Description: cfg.Description,
		Instruction: instruction,
		Model:       cfg.Model,
		Tools:       tools,
	})
}

// rosterOrder yields specialist names in cfg.Order first (restricted
// to built specialists, de-duplicated), then any remaining specialists
// in sorted order so the result is deterministic.
func rosterOrder(cfg Config) []string {
	out := make([]string, 0, len(cfg.Specialists))
	seen := make(map[string]bool, len(cfg.Specialists))
	for _, name := range cfg.Order {
		if _, ok := cfg.Specialists[name]; ok && !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	rest := make([]string, 0, len(cfg.Specialists))
	for name := range cfg.Specialists {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	// Insertion sort keeps this dependency-free; rosters are small.
	for i := 1; i < len(rest); i++ {
		for j := i; j > 0 && rest[j] < rest[j-1]; j-- {
			rest[j], rest[j-1] = rest[j-1], rest[j]
		}
	}
	return append(out, rest...)
}

// NotImplemented is the structured result every scaffolded-but-unwired
// vocabulary tool returns in v0.1. It is a tool RESULT rather than a
// Go error so the planner LLM receives a deterministic, parseable
// response (a Go error would surface as {"error": ...} and invite
// retries); ErrNotImplemented is the programmatic counterpart for
// mast-side callers.
func NotImplemented(toolName string) map[string]any {
	return map[string]any{
		"status": "error",
		"error":  "not_implemented",
		"tool":   toolName,
		"detail": "reference-graph shape tools are scaffolded in v0.1 and wired in v0.2 (docs/orchestration-design.md, phasing). Compose with invoke_specialist instead, or finish_task with what you have.",
	}
}

// ErrNotImplemented mirrors the NotImplemented tool result for Go
// callers probing the vocabulary programmatically.
var ErrNotImplemented = fmt.Errorf("planner: tool not implemented in v0.1 scaffold")

// runShapeLLMRouterArgs is the pinned v0.1 schema of
// run_shape_llm_router (docs/orchestration-design.md tool-vocabulary
// table: `run_shape_llm_router(classifier, handlers)`).
type runShapeLLMRouterArgs struct {
	// Classifier is the name of a bundle-scoped SingleTurn specialist.
	Classifier string `json:"classifier"`
	// Handlers are the specialist names routed to, one per class.
	Handlers []string `json:"handlers"`
}

// runShapeFanOutFanInArgs is the pinned v0.1 schema of
// run_shape_fan_out_fan_in (docs/orchestration-design.md
// tool-vocabulary table: `run_shape_fan_out_fan_in(planner_fn,
// workers, joiner)`).
type runShapeFanOutFanInArgs struct {
	// PlannerFn describes how to split the work into items.
	PlannerFn string `json:"planner_fn"`
	// Workers are specialist names from the bundle roster.
	Workers []string `json:"workers"`
	// Joiner is the specialist that folds worker results.
	Joiner string `json:"joiner"`
}

func newNotImplementedShapeTool[TArgs any](name, description string, handler func(adkagent.Context, TArgs) (map[string]any, error)) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        name,
		Description: description + "\n\nNOT IMPLEMENTED in this build (v0.1 scaffold): calling it returns a structured not_implemented error. Prefer invoke_specialist.",
	}, handler)
}

// DefaultInstructionTemplate is the planner's default system prompt
// (docs/orchestration-design.md "The planner": supervisor-body turn
// loop, plan-first gate as a first-turn prose plan, finish_task exit).
// Rendered with the roster; Config.Instruction overrides it wholesale.
const DefaultInstructionTemplate = `You are the planner for the {{.Workload}} workload, running unattended:
no one is watching the session and no one answers mid-task questions
except through the request_operator_input escalation tool. You do not
do the work yourself — you construct the execution turn by turn from
the vocabulary below, one tool call per turn, and every decision you
record in text becomes part of the audit log.

First turn: before any tool call, state a short prose plan of intended
composition — which specialists (and, once available, which reference
shapes) you expect to invoke, in what order, and what would make you
escalate. Then execute the plan one tool call per turn, folding each
tool result into the next decision.

Vocabulary:
- invoke_specialist(name, input): run one specialist from the roster:
  {{.Roster}}. Give it a self-contained input; you receive its
  structured result.
- run_shape_llm_router / run_shape_fan_out_fan_in: reference-graph
  shapes. In this build they return a structured not_implemented
  error — do not retry them; compose specialists directly instead.
- request_operator_input(message, schema): escalate to a human
  operator and pause until they respond. Use it when you hit genuine
  ambiguity or an action that needs judgment; say exactly what
  decision you need.
- finish_task(result): your only exit. Finish with a structured
  result: what was done, what was found, what remains.

Work with conservative defaults: prefer reversible actions, never
widen the task beyond what was given, and fail fast — a clean early
finish_task reporting what is blocked beats a confident wrong action.`

type instructionData struct {
	Workload string
	Roster   string
}

func renderInstruction(cfg Config, roster []string) (string, error) {
	tmpl, err := template.New("planner-instruction").Parse(DefaultInstructionTemplate)
	if err != nil {
		return "", err
	}
	rosterText := "(none declared)"
	if len(roster) > 0 {
		rosterText = strings.Join(roster, ", ")
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, instructionData{Workload: cfg.Name, Roster: rosterText}); err != nil {
		return "", err
	}
	return b.String(), nil
}
