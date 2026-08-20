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

// Package toolcatalog assembles the tool declarations a real mast turn
// puts in front of a model, and states what each one promises the model
// it accepts. It exists so a provider adapter can be asserted against
// the whole catalog rather than against one hand-built fixture.
//
// #154 was a total tool-calling outage on the Anthropic path: every
// tool built by ADK's functiontool.New or by pkg/mcp reached Claude as
// a name with an empty input schema, because the converter handled only
// the typed genai.Schema spelling. The regression test that shipped
// with the fix covers functiontool.New, which is the shape of that bug
// rather than the shape of the bug class — the standing lesson is that
// hardening has to cover every construction path (#168).
//
// So the catalog is not a list. It is **captured from a running
// agent**: two rigs are driven through a real ADK runner with a
// recording model, and what the model was handed is what gets
// asserted. A construction path cannot be forgotten here, because
// nothing enumerates them — a new tool wired into either rig's shape
// appears in the catalog the next time the test runs. What the rigs
// produce today spans the three paths that matter:
//
//   - typed Parameters (*genai.Schema) — ADK's finish_task, and the
//     per-sub-agent delegation tool llmagent installs on a coordinator.
//     This is the path that always worked. transfer_to_agent, ADK's
//     other typed-Parameters tool, is deliberately absent: mast's
//     specialists are Task-mode and llmagent's transferTargets skips
//     those, so no mast shape offers it, and manufacturing one to reach
//     it would put a rig in the catalog that production never builds.
//   - ParametersJsonSchema via functiontool.New — every mast-authored
//     tool: the planner vocabulary, pause_session, the federation
//     invoke tool. This is the path #154 broke.
//   - ParametersJsonSchema via pkg/mcp — an MCP server's advertised
//     inputSchema, which is where a workload's real read and write
//     tools come from.
//
// The package takes no dependency on testing: it returns errors, so a
// caller in any test package can drive it, and the MCP endpoint is
// passed in rather than stood up here.
package toolcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"sort"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/federation"
	"github.com/go-steer/mast/pkg/mcp"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/transcript"
)

// Entry is one tool as the model was offered it, plus what its own
// declaration promises. Props and Required are derived from the
// declaration rather than written down, so the expectations cannot
// drift from the tools: a tool that gains an argument gains it here on
// the next run.
type Entry struct {
	// Name is the function name the model calls.
	Name string
	// Spelling records which declaration field carried the schema —
	// "Parameters" (typed *genai.Schema) or "ParametersJsonSchema".
	// Reported in failures because it names the converter branch.
	Spelling string
	// Rig names the agent shape that offered this tool, so a failure
	// says where to look.
	Rig string
	// Declaration is the genai declaration ADK handed the model,
	// verbatim. This is the input a provider adapter converts.
	Declaration *genai.FunctionDeclaration
	// Props are the argument names the declaration advertises, sorted.
	// Empty means the tool genuinely takes no arguments — a fact, not
	// a hole, and the invariant treats it as one.
	Props []string
	// Required are the argument names the declaration marks required,
	// sorted. A required argument that does not survive the conversion
	// is worse than a missing optional one: the model cannot call the
	// tool at all without inventing the name.
	Required []string
}

// HasParams reports whether the tool advertises any arguments. Tools
// that do not are still catalogued — a converter that invents
// properties for a no-arg tool is also a defect — but the "properties
// must not be empty" check only applies to those that do.
func (e Entry) HasParams() bool { return len(e.Props) > 0 }

// Config parameterizes the rigs.
type Config struct {
	// MCPEndpoint is the streamable-HTTP URL of an MCP server to
	// enumerate. Required: the MCP path is one of the three, and
	// skipping it silently would defeat the point. Callers stand up
	// an in-process server (net/http/httptest) — no network.
	MCPEndpoint string
}

// Build drives the rigs and returns the catalog, sorted by name.
//
// A tool offered by more than one rig appears once; the declaration is
// the same object either way, and a duplicate would only inflate the
// failure output.
func Build(ctx context.Context, cfg Config) ([]Entry, error) {
	if cfg.MCPEndpoint == "" {
		return nil, fmt.Errorf("toolcatalog: MCPEndpoint is required (the MCP construction path is one of the three this catalog exists to cover)")
	}

	byName := map[string]Entry{}
	collect := func(rig string, decls []*genai.FunctionDeclaration) error {
		for _, d := range decls {
			if d == nil || d.Name == "" {
				continue
			}
			if _, dup := byName[d.Name]; dup {
				continue
			}
			e, err := entryFor(rig, d)
			if err != nil {
				return err
			}
			byName[d.Name] = e
		}
		return nil
	}

	coordDecls, err := coordinatorRig(ctx, cfg.MCPEndpoint)
	if err != nil {
		return nil, err
	}
	if err := collect("coordinator+mcp", coordDecls); err != nil {
		return nil, err
	}

	planDecls, err := plannerRig(ctx)
	if err != nil {
		return nil, err
	}
	if err := collect("planner", planDecls); err != nil {
		return nil, err
	}

	out := make([]Entry, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// entryFor reads a declaration's advertised arguments out of whichever
// field carries them. Both spellings are read through JSON rather than
// through the typed accessors, because that is what every converter
// does and reading it the same way keeps the expectation honest about
// the bytes rather than about the Go types.
func entryFor(rig string, d *genai.FunctionDeclaration) (Entry, error) {
	e := Entry{Name: d.Name, Rig: rig, Declaration: d}
	var src any
	switch {
	case d.Parameters != nil:
		e.Spelling = "Parameters"
		src = d.Parameters
	case d.ParametersJsonSchema != nil:
		e.Spelling = "ParametersJsonSchema"
		src = d.ParametersJsonSchema
	default:
		e.Spelling = "none"
		return e, nil
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return Entry{}, fmt.Errorf("toolcatalog: tool %q: marshal %s: %w", d.Name, e.Spelling, err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return Entry{}, fmt.Errorf("toolcatalog: tool %q: unmarshal %s: %w", d.Name, e.Spelling, err)
	}
	if props, ok := generic["properties"].(map[string]any); ok {
		for k := range props {
			e.Props = append(e.Props, k)
		}
	}
	if req, ok := generic["required"].([]any); ok {
		for _, v := range req {
			if s, ok := v.(string); ok {
				e.Required = append(e.Required, s)
			}
		}
	}
	sort.Strings(e.Props)
	sort.Strings(e.Required)
	return e, nil
}

// coordinatorRig is the attach/coordinator shape: a Chat coordinator
// with a Task sub-agent (ADK installs transfer_to_agent and the
// sub-agent's task tool), a functiontool.New tool, and a live MCP
// toolset. Three construction paths in one request.
func coordinatorRig(ctx context.Context, endpoint string) ([]*genai.FunctionDeclaration, error) {
	ts, err := mcp.NewToolset(ctx, "catalog-mcp", mcp.ServerConfig{
		Transport: mcp.TransportHTTP,
		URL:       endpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("toolcatalog: mcp toolset: %w", err)
	}

	fed, err := federation.NewInvokeRemoteAgentTool(federation.NewRegistry())
	if err != nil {
		return nil, fmt.Errorf("toolcatalog: federation tool: %w", err)
	}

	sub, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        "catalog_task",
		Description: "Task sub-agent, so ADK installs its transfer and task tools.",
		Model:       &recorder{},
	})
	if err != nil {
		return nil, fmt.Errorf("toolcatalog: task sub-agent: %w", err)
	}

	rec := &recorder{}
	root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
		Name:        "catalog_coordinator",
		Description: "Coordinator rig for the tool-declaration catalog.",
		Model:       rec,
		SubAgents:   []adkagent.Agent{sub},
		Tools:       []tool.Tool{fed},
		Toolsets:    []tool.Toolset{ts},
	})
	if err != nil {
		return nil, fmt.Errorf("toolcatalog: coordinator: %w", err)
	}
	return runAndCapture(ctx, "catalog-coordinator", root, rec)
}

// plannerRig is the orchestrate shape: the planner's own vocabulary,
// every entry of it a functiontool.New declaration, plus the
// finish_task ADK installs for Task mode. pause_session is only
// registered when a PauseRecorder is set, so the rig sets one — a
// vocabulary that omits the one tool with an enum argument would be a
// thinner catalog than production's.
func plannerRig(ctx context.Context) ([]*genai.FunctionDeclaration, error) {
	rec := &recorder{}
	specialist, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        "catalog_specialist",
		Description: "Roster entry, so invoke_specialist has a target.",
		Model:       &recorder{},
	})
	if err != nil {
		return nil, fmt.Errorf("toolcatalog: specialist: %w", err)
	}
	root, err := planner.NewRoot(planner.Config{
		Name:          "catalog",
		Description:   "Planner rig for the tool-declaration catalog.",
		Model:         rec,
		Specialists:   map[string]adkagent.Agent{"catalog_specialist": specialist},
		Order:         []string{"catalog_specialist"},
		PauseRecorder: noopPauseRecorder{},
	})
	if err != nil {
		return nil, fmt.Errorf("toolcatalog: planner root: %w", err)
	}
	return runAndCapture(ctx, "catalog-planner", root, rec)
}

// runAndCapture drives one user turn through a real runner and returns
// the function declarations the recording model was handed. Going
// through the runner rather than reading the agent's Tools field is
// deliberate: the tools ADK installs itself (transfer_to_agent,
// finish_task, the per-sub-agent task tools) exist only in the
// assembled request, and those are exactly the typed-Parameters
// declarations the catalog needs on the other side of the comparison.
func runAndCapture(ctx context.Context, appName string, root adkagent.Agent, rec *recorder) ([]*genai.FunctionDeclaration, error) {
	r, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("toolcatalog: runner.New: %w", err)
	}
	msg := genai.NewContentFromText("enumerate", genai.RoleUser)
	for _, err := range r.Run(ctx, "catalog-user", "catalog-session", msg, adkagent.RunConfig{}) {
		if err != nil {
			return nil, fmt.Errorf("toolcatalog: run %s: %w", appName, err)
		}
	}
	if len(rec.decls) == 0 {
		return nil, fmt.Errorf("toolcatalog: rig %s handed the model no tools; the catalog would be vacuously green", appName)
	}
	return rec.decls, nil
}

// recorder is a model that answers nothing and remembers the tools it
// was offered. It must terminate the turn on the first call — a model
// that emits a function call would drive the agent onward and the rig
// would be measuring a second request instead of the first.
type recorder struct {
	decls []*genai.FunctionDeclaration
}

func (m *recorder) Name() string { return "toolcatalog-recorder" }

func (m *recorder) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	if req != nil && req.Config != nil {
		for _, t := range req.Config.Tools {
			if t == nil {
				continue
			}
			m.decls = append(m.decls, t.FunctionDeclarations...)
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "catalogued"}},
			},
			TurnComplete: true,
		}, nil)
	}
}

// noopPauseRecorder satisfies planner.PauseRecorder so pause_session
// joins the vocabulary. It is never called: the recording model never
// emits the call.
type noopPauseRecorder struct{}

func (noopPauseRecorder) PauseInterrupt(context.Context, string, string, string, transcript.PauseSpec) (transcript.PauseHandle, error) {
	return transcript.PauseHandle{}, fmt.Errorf("toolcatalog: pause recorder is a stub and must not be called")
}
