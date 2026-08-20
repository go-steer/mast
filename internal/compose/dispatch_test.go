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
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/graph"
	"github.com/go-steer/mast/pkg/planner"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// Resolve is the one place the two ways of naming a dispatch shape meet:
// the caller's (the --dispatch flag, or a library caller's field) and
// the bundle's own `dispatch:`. The precedence has to be exactly this,
// because a bundle declaring `dispatch: fanout` is stating a fact about
// its roster while a flag is one operator overriding one run.
func TestDispatchResolve(t *testing.T) {
	fanoutBundle := workload.Bundle{Dispatch: workload.DispatchFanout}
	plain := workload.Bundle{}

	tests := []struct {
		name   string
		caller Dispatch
		bundle workload.Bundle
		want   Dispatch
	}{
		{"caller wins over the bundle", DispatchGraph, fanoutBundle, DispatchGraph},
		{"bundle wins over an unset caller", "", fanoutBundle, DispatchFanout},
		{"bundle wins over an explicit auto", DispatchAuto, fanoutBundle, DispatchFanout},
		{"unset caller, silent bundle -> auto", "", plain, DispatchAuto},
		{"explicit auto, silent bundle -> auto", DispatchAuto, plain, DispatchAuto},
		{"explicit caller, silent bundle", DispatchCoordinator, plain, DispatchCoordinator},
		// An unknown value resolves rather than being silently dropped;
		// BuildRoot's switch is what rejects it, so the error names what
		// was actually asked for.
		{"an unknown bundle value survives to be rejected", "", workload.Bundle{Dispatch: "sideways"}, Dispatch("sideways")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.caller.Resolve(tc.bundle); got != tc.want {
				t.Fatalf("Resolve = %q, want %q", got, tc.want)
			}
		})
	}
}

// fanoutSpecs is a roster BuildFanout accepts: analysts that enumerate
// a read-only tool, plus the synthesis merger.
func fanoutSpecs(names ...string) []specialists.Spec {
	specs := make([]specialists.Spec, 0, len(names)+1)
	for _, n := range names {
		specs = append(specs, specialists.Spec{
			Name:        n,
			Instruction: n,
			Mode:        specialists.ModeTask,
			Tools: specialists.ToolAllowlist{
				MCP: []specialists.MCPAllowlist{{Server: "gke", Tools: []string{"get_k8s_resource"}}},
			},
		})
	}
	return append(specs, specialists.Spec{
		Name:        graph.SynthesisName,
		Instruction: "merge",
		Mode:        specialists.ModeTask,
		// The merger reads its branches' findings, not the cluster, so
		// it declares the deny-all MCP allowlist (`mcp: []` in a .tmpl).
		// Leaving the field nil would mean "inherit the whole catalog"
		// and CheckCapabilitySplit would refuse the roster.
		Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{}},
	})
}

func readOnlyCatalog() workload.ToolCatalog {
	no := false
	return workload.ToolCatalog{
		MCP:   []workload.MCPServerRef{{Server: "gke"}},
		Tools: []workload.ToolPolicy{{Name: "get_k8s_resource", Mutating: &no}},
	}
}

func TestBuildRootFanoutFromBundleDispatch(t *testing.T) {
	root, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle: workload.Bundle{
			Name:        "ns-audit",
			Dispatch:    workload.DispatchFanout,
			ToolCatalog: readOnlyCatalog(),
		},
		Specs: fanoutSpecs("alpha", "beta"),
		Model: mastagent.NewEchoModel("echo"),
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	if root.Name() != "ns-audit_fanout" {
		t.Fatalf("root = %q, want ns-audit_fanout — the bundle's dispatch: did not reach the shape", root.Name())
	}
}

// A synthesis specialist in the roster is what makes a roster a fan-out
// roster, so auto picks fan-out from the roster alone. This is the
// library default: a programmatic caller declares specialists, not a
// dispatch string.
func TestBuildRootAutoPicksFanoutFromTheRoster(t *testing.T) {
	root, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle:   workload.Bundle{Name: "w", ToolCatalog: readOnlyCatalog()},
		Specs:    fanoutSpecs("alpha"),
		Model:    mastagent.NewEchoModel("echo"),
		Dispatch: DispatchAuto,
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	if root.Name() != "w_fanout" {
		t.Fatalf("root = %q, want w_fanout", root.Name())
	}
}

// The synthesis specialist is not itself an analyst. Without the
// exclusion it would fan out alongside the others and then merge its
// own finding — and, having no tool allowlist of its own, it would be
// refused by the branch check first, so this is load-bearing for the
// happy path above as well.
//
// The assertion is on the built tree rather than on a count, because
// the roster does not hang off the root: the analysts are the fan
// agent's sub-agents (ADK refuses an agent with two parents), so the
// root registers the fan and the merger and nothing else.
func TestBuildRootFanoutExcludesSynthesisFromTheBranches(t *testing.T) {
	root, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle:   workload.Bundle{Name: "w", ToolCatalog: readOnlyCatalog()},
		Specs:    fanoutSpecs("alpha", "beta"),
		Model:    mastagent.NewEchoModel("echo"),
		Dispatch: DispatchFanout,
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	if got, want := agentNames(root.SubAgents()), []string{"w_fan", graph.SynthesisName}; !slices.Equal(got, want) {
		t.Fatalf("root sub-agents = %v, want %v", got, want)
	}
	var fan adkagent.Agent
	for _, sub := range root.SubAgents() {
		if sub.Name() == "w_fan" {
			fan = sub
		}
	}
	// Both analysts fan out, wrapped; the merger does not.
	got := agentNames(fan.SubAgents())
	want := []string{graph.BranchPrefix + "alpha", graph.BranchPrefix + "beta"}
	if !slices.Equal(got, want) {
		t.Fatalf("branches = %v, want %v", got, want)
	}
}

func agentNames(agents []adkagent.Agent) []string {
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name())
	}
	return names
}

// The workload's tool_catalog classifications have to reach the branch
// check, or every fan-out roster is refused: an MCP tool is mutating
// until the bundle says otherwise, so an empty catalog makes the very
// same roster unbuildable.
func TestBuildRootFanoutCarriesTheCatalogClassifications(t *testing.T) {
	_, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle: workload.Bundle{
			Name:     "w",
			Dispatch: workload.DispatchFanout,
			ToolCatalog: workload.ToolCatalog{
				MCP: []workload.MCPServerRef{{Server: "gke"}},
			},
		},
		Specs: fanoutSpecs("alpha"),
		Model: mastagent.NewEchoModel("echo"),
	})
	if err == nil || !strings.Contains(err.Error(), "get_k8s_resource") {
		t.Fatalf("want a refusal naming the unclassified tool, got %v", err)
	}
}

func TestRosterShape(t *testing.T) {
	classifier := specialists.Spec{Name: "clf", Mode: specialists.ModeSingleTurn}
	fallback := specialists.Spec{Name: graph.FallbackName, Mode: specialists.ModeTask}
	synthesis := specialists.Spec{Name: graph.SynthesisName, Mode: specialists.ModeTask}
	plain := specialists.Spec{Name: "a", Mode: specialists.ModeTask}

	tests := []struct {
		name  string
		specs []specialists.Spec
		want  Dispatch
	}{
		{"a merger makes it a fan-out roster", []specialists.Spec{plain, synthesis}, DispatchFanout},
		{"classifier plus fallback is the graph pair", []specialists.Spec{classifier, plain, fallback}, DispatchGraph},
		{"a classifier with no fallback is not the pair", []specialists.Spec{classifier, plain}, DispatchCoordinator},
		{"a fallback with no classifier is not the pair", []specialists.Spec{plain, fallback}, DispatchCoordinator},
		{"a plain roster is a coordinator", []specialists.Spec{plain}, DispatchCoordinator},
		{"an empty roster is a coordinator", nil, DispatchCoordinator},
		// Fan-out wins: a roster carrying both a merger and the graph
		// pair has said the more specific thing.
		{"a merger outranks the graph pair", []specialists.Spec{classifier, fallback, synthesis}, DispatchFanout},
		// Mode matters, not just the name — an empty Mode is Task (the
		// same default specialists.Build applies), so a programmatic
		// roster shapes like a loaded one.
		{"an unset mode counts as Task", []specialists.Spec{{Name: graph.SynthesisName}}, DispatchFanout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RosterShape(tc.specs); got != tc.want {
				t.Fatalf("RosterShape = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildRootRejectsUnknownDispatch(t *testing.T) {
	_, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle: workload.Bundle{Name: "w", Dispatch: "sideways"},
		Specs:  fanoutSpecs("alpha"),
		Model:  mastagent.NewEchoModel("echo"),
	})
	if err == nil || !strings.Contains(err.Error(), "sideways") {
		t.Fatalf("want an unknown-dispatch error naming the value, got %v", err)
	}
}

// The daemon's /tools endpoint reports what mast wired, and mast's
// non-MCP tools are the planner's control plane (#137). Before this, a
// planner-dispatch daemon with no MCP servers answered with an empty
// catalog — true of its MCP tools, and the wrong answer to "what can
// this daemon do".
//
// The names are asserted rather than the count, because the count is
// what a hand-maintained list would also get right.
func TestBuildRootReportsThePlannerVocabulary(t *testing.T) {
	root, builtin, err := BuildRoot(context.Background(), RootConfig{
		Bundle: workload.Bundle{
			Name:        "triage",
			ToolCatalog: readOnlyCatalog(),
			Planner:     workload.Planner{Enabled: true},
		},
		Specs: plannerSpecs(),
		Model: mastagent.NewEchoModel("echo"),
	})
	if err != nil {
		t.Fatalf("BuildRoot: %v", err)
	}
	if root == nil {
		t.Fatal("BuildRoot returned no root")
	}
	want := []string{
		planner.ToolInvokeSpecialist,
		planner.ToolRunShapeLLMRouter,
		planner.ToolRunShapeFanOutFanIn,
		planner.ToolRequestOperatorInput,
	}
	if got := toolNames(builtin); !slices.Equal(got, want) {
		t.Fatalf("builtin tools = %v, want %v", got, want)
	}
}

// pause_session is registered only when a PauseRecorder is wired, so
// the catalog has to follow the config rather than a fixed list — a
// daemon with no durable store must not advertise a pause it cannot
// take.
func TestBuildRootReportsPauseSessionOnlyWhenItIsWired(t *testing.T) {
	build := func(rec planner.PauseRecorder) []string {
		t.Helper()
		_, builtin, err := BuildRoot(context.Background(), RootConfig{
			Bundle: workload.Bundle{
				Name:        "triage",
				ToolCatalog: readOnlyCatalog(),
				Planner:     workload.Planner{Enabled: true},
			},
			Specs:         plannerSpecs(),
			Model:         mastagent.NewEchoModel("echo"),
			PauseRecorder: rec,
		})
		if err != nil {
			t.Fatalf("BuildRoot: %v", err)
		}
		return toolNames(builtin)
	}
	if got := build(nil); slices.Contains(got, planner.ToolPauseSession) {
		t.Errorf("no recorder wired, but the catalog offers %s: %v", planner.ToolPauseSession, got)
	}
	if got := build(stubPauseRecorder{}); !slices.Contains(got, planner.ToolPauseSession) {
		t.Errorf("a recorder is wired, but %s is missing from %v", planner.ToolPauseSession, got)
	}
}

// Every other shape reports nothing, and that is the honest answer
// rather than a hole: compose wires no tools of its own onto them, and
// what ADK installs (finish_task, a coordinator's delegation tools) is
// behind an accessor that does not exist — #51 / adk-go#1229. Claiming
// otherwise here would be the drift a hand-maintained list produces.
func TestBuildRootReportsNoBuiltinsUnderOtherDispatch(t *testing.T) {
	for _, d := range []Dispatch{DispatchFanout, DispatchCoordinator} {
		t.Run(string(d), func(t *testing.T) {
			_, builtin, err := BuildRoot(context.Background(), RootConfig{
				Bundle:   workload.Bundle{Name: "w", ToolCatalog: readOnlyCatalog()},
				Specs:    fanoutSpecs("alpha"),
				Model:    mastagent.NewEchoModel("echo"),
				Dispatch: d,
			})
			if err != nil {
				t.Fatalf("BuildRoot: %v", err)
			}
			if len(builtin) != 0 {
				t.Errorf("builtin tools = %v, want none under %s dispatch", toolNames(builtin), d)
			}
		})
	}
}

// plannerSpecs is a one-specialist roster that satisfies the read/write
// split: an empty mcp allowlist means "needs no cluster tools", which is
// true of a stand-in and keeps CheckCapabilitySplit out of the way of
// what this file is testing.
func plannerSpecs() []specialists.Spec {
	return []specialists.Spec{{
		Name:        "alpha",
		Instruction: "look",
		Mode:        specialists.ModeTask,
		Tools:       specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{}},
	}}
}

func toolNames(tools []tool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}

// stubPauseRecorder is enough to register pause_session; it is never
// called, because nothing here runs a turn.
type stubPauseRecorder struct{}

func (stubPauseRecorder) PauseInterrupt(context.Context, string, string, string, transcript.PauseSpec) (transcript.PauseHandle, error) {
	return transcript.PauseHandle{}, errors.New("compose test: pause recorder is a stub and must not be called")
}
