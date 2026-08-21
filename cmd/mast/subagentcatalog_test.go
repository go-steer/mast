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

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/graph"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

func spec(name string, mode specialists.Mode) specialists.Spec {
	return specialists.Spec{Name: name, Mode: mode}
}

// Invocation is the field core-agent's `modes` cannot carry: which of
// mast's four ways in reaches this specialist. Each shape routes a
// different subset of the same roster, so the same spec list has to
// project differently under each.
func TestSubagentCatalogInvocationPerShape(t *testing.T) {
	roster := []specialists.Spec{
		spec("classifier", specialists.ModeSingleTurn),
		spec("log-analyst", specialists.ModeTask),
		spec(graph.SynthesisName, specialists.ModeTask),
		spec(graph.FallbackName, specialists.ModeTask),
	}

	cases := []struct {
		name     string
		bundle   *workload.Bundle
		dispatch string
		want     map[string]string
	}{
		{
			name:     "coordinator routes to everything by transfer",
			dispatch: workload.DispatchCoordinator,
			want: map[string]string{
				"classifier":        attach.InvocationTransfer,
				"log-analyst":       attach.InvocationTransfer,
				graph.SynthesisName: attach.InvocationTransfer,
				graph.FallbackName:  attach.InvocationTransfer,
			},
		},
		{
			name:     "graph builds every Task spec as a node, the first SingleTurn as the router",
			dispatch: workload.DispatchGraph,
			want: map[string]string{
				"classifier":        attach.InvocationGraphNode,
				"log-analyst":       attach.InvocationGraphNode,
				graph.SynthesisName: attach.InvocationGraphNode,
				graph.FallbackName:  attach.InvocationGraphNode,
			},
		},
		{
			// The distinction an operator reading a fanout roster
			// wants: which members run concurrently and which one
			// merges them. And which one nothing reaches at all —
			// BuildFanout builds no fallback node and has no
			// classifier, so those two are orphans under this shape.
			name:     "fanout separates branches, merger, and orphans",
			dispatch: workload.DispatchFanout,
			want: map[string]string{
				"classifier":        "",
				"log-analyst":       attach.InvocationFanoutBranch,
				graph.SynthesisName: attach.InvocationGraphNode,
				graph.FallbackName:  "",
			},
		},
		{
			// planner.enabled overrides --dispatch entirely in
			// compose, so the catalog has to as well.
			name:     "planner overrides the dispatch flag",
			bundle:   &workload.Bundle{Planner: workload.Planner{Enabled: true}},
			dispatch: workload.DispatchFanout,
			want: map[string]string{
				"classifier":        attach.InvocationParentTool,
				"log-analyst":       attach.InvocationParentTool,
				graph.SynthesisName: attach.InvocationParentTool,
				graph.FallbackName:  attach.InvocationParentTool,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, e := range subagentCatalog(tc.bundle, roster, tc.dispatch) {
				got[e.Name] = e.Invocation
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("invocations\n got: %v\nwant: %v", got, tc.want)
			}
		})
	}
}

// `modes` is core-agent's vocabulary, and mast may only say what is
// true in it. Under planner dispatch a specialist really is carried as
// a parent tool, so "sync" is earned; under every other shape it is
// neither spawnable by reference nor a parent tool, and the list is
// empty. Emitting "async" to make the field look populated is the
// upstream defect (core-agent#741) this endpoint was warned about.
func TestSubagentCatalogModesClaimOnlyWhatIsTrue(t *testing.T) {
	roster := []specialists.Spec{spec("log-analyst", specialists.ModeTask)}

	planner := subagentCatalog(&workload.Bundle{Planner: workload.Planner{Enabled: true}}, roster, "")
	if want := []string{attach.SubagentInvocationModeSync}; !reflect.DeepEqual(planner[0].Modes, want) {
		t.Errorf("planner modes = %v, want %v", planner[0].Modes, want)
	}

	for _, shape := range []string{workload.DispatchCoordinator, workload.DispatchGraph, workload.DispatchFanout} {
		got := subagentCatalog(nil, roster, shape)
		if len(got[0].Modes) != 0 {
			t.Errorf("%s modes = %v, want empty — nothing in mast is spawnable by reference", shape, got[0].Modes)
		}
		// Empty, not nil: the field has no omitempty, and a client
		// reading `"modes": null` cannot tell it from a missing field.
		if got[0].Modes == nil {
			t.Errorf("%s modes is nil; want an empty list", shape)
		}
	}
}

// `auto` is resolved by the same roster read compose uses, so the
// catalog describes the shape that was actually built rather than the
// word the operator typed.
func TestSubagentCatalogResolvesAutoDispatch(t *testing.T) {
	// A synthesis specialist present is what makes RosterShape pick
	// fanout.
	roster := []specialists.Spec{
		spec("log-analyst", specialists.ModeTask),
		spec(graph.SynthesisName, specialists.ModeTask),
	}
	for _, dispatch := range []string{workload.DispatchAuto, ""} {
		got := subagentCatalog(nil, roster, dispatch)
		if got[0].Invocation != attach.InvocationFanoutBranch {
			t.Errorf("dispatch %q: log-analyst invocation = %q, want %q",
				dispatch, got[0].Invocation, attach.InvocationFanoutBranch)
		}
	}
}

// The mast-native fields are the answer to "what can this daemon do":
// which member can touch the cluster, and what shape of agent it is.
func TestSubagentCatalogCarriesDeclaredFields(t *testing.T) {
	roster := []specialists.Spec{{
		Name:        "change-executor",
		Description: "applies approved changes",
		Mode:        specialists.ModeTask,
		Model:       "gemini-2.5-pro",
		Capability:  specialists.CapabilityChangeExecutor,
		Filename:    "specialists/change-executor.tmpl",
	}}

	got := subagentCatalog(nil, roster, workload.DispatchCoordinator)
	want := attach.SubagentCatalogInfo{
		Name:        "change-executor",
		Description: "applies approved changes",
		Model:       "gemini-2.5-pro",
		Root:        "specialists/change-executor.tmpl",
		Modes:       []string{},
		Invocation:  attach.InvocationTransfer,
		Capability:  string(specialists.CapabilityChangeExecutor),
		AgentMode:   string(specialists.ModeTask),
		Tools:       attach.SubagentToolGrant{MCPGrant: attach.MCPGrantAll},
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("entry\n got: %+v\nwant: %+v", got[0], want)
	}

	// And the wire form a client actually parses.
	blob, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const wantJSON = `{"name":"change-executor","description":"applies approved changes","model":"gemini-2.5-pro",` +
		`"root":"specialists/change-executor.tmpl","modes":[],"invocation":"transfer",` +
		`"capability":"change_executor","agent_mode":"Task","tools":{"mcp_grant":"all"}}`
	if string(blob) != wantJSON {
		t.Errorf("wire form\n got: %s\nwant: %s", blob, wantJSON)
	}
}

// The MCP axis is read on presence, and the two presences that matter
// are one character apart: no `mcp:` key grants every toolset the
// workload has, `mcp: []` grants none. Both decode to an empty array,
// so a catalog that transcribed the list would report the deny-all
// specialist and the inherit-everything one identically — and it is the
// inherit-everything one that can reach the cluster. mcp_grant names
// which was declared.
func TestTheMCPAxisReportsPresenceNotSyntax(t *testing.T) {
	cases := []struct {
		name string
		mcp  []specialists.MCPAllowlist
		want string
	}{
		{"no mcp: key inherits every toolset", nil, attach.MCPGrantAll},
		{"`mcp: []` denies them all", []specialists.MCPAllowlist{}, attach.MCPGrantNone},
		{
			"a non-empty list is a whitelist",
			[]specialists.MCPAllowlist{{Server: "gke", Tools: []string{"get_pod"}}},
			attach.MCPGrantListed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := spec("log-analyst", specialists.ModeTask)
			s.Tools.MCP = tc.mcp
			got := subagentCatalog(nil, []specialists.Spec{s}, workload.DispatchCoordinator)
			if got[0].Tools.MCPGrant != tc.want {
				t.Errorf("mcp_grant = %q, want %q", got[0].Tools.MCPGrant, tc.want)
			}
			// The per-server detail is meaningful only under "listed";
			// carrying it under the others would invite a client to read
			// an empty list as the grant.
			if tc.want != attach.MCPGrantListed && len(got[0].Tools.MCP) != 0 {
				t.Errorf("mcp = %+v under grant %q, want nothing", got[0].Tools.MCP, tc.want)
			}
		})
	}
}

// The same erasure one level down: filterToolsets passes a listed
// server whole when the entry declares no tools[] of its own, so
// "narrowed to nothing" and "not narrowed at all" are again the same
// empty array. whole_server says which.
func TestAListedServerWithNoToolsPassesWhole(t *testing.T) {
	s := spec("log-analyst", specialists.ModeTask)
	s.Tools.MCP = []specialists.MCPAllowlist{
		{Server: "gke", Tools: []string{"get_pod", "get_events"}},
		{Server: "slack"},
	}

	got := subagentCatalog(nil, []specialists.Spec{s}, workload.DispatchCoordinator)
	want := []attach.SubagentMCPGrant{
		{Server: "gke", Tools: []string{"get_pod", "get_events"}},
		{Server: "slack", WholeServer: true},
	}
	if !reflect.DeepEqual(got[0].Tools.MCP, want) {
		t.Errorf("mcp\n got: %+v\nwant: %+v", got[0].Tools.MCP, want)
	}

	blob, err := json.Marshal(got[0].Tools)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const wantJSON = `{"mcp_grant":"listed","mcp":[` +
		`{"server":"gke","tools":["get_pod","get_events"],"whole_server":false},` +
		`{"server":"slack","whole_server":true}]}`
	if string(blob) != wantJSON {
		t.Errorf("wire form\n got: %s\nwant: %s", blob, wantJSON)
	}
}

// tools.builtin is reported under a name that says it is a declaration,
// because that is all it is in mast: nothing populates
// specialists.BuildOptions.Tools, so a specialist is built holding no
// built-in tools at all and the list narrows nothing. What does read it
// is the write gate and the capability-split check. Publishing it as
// "builtin" would tell an operator the specialist can call these.
func TestTheBuiltinAxisIsReportedAsADeclaration(t *testing.T) {
	s := spec("change-executor", specialists.ModeTask)
	s.Tools.Builtin = []string{"apply_manifest"}

	got := subagentCatalog(nil, []specialists.Spec{s}, workload.DispatchCoordinator)
	if want := []string{"apply_manifest"}; !reflect.DeepEqual(got[0].Tools.BuiltinDeclared, want) {
		t.Errorf("builtin_declared = %v, want %v", got[0].Tools.BuiltinDeclared, want)
	}

	blob, err := json.Marshal(got[0].Tools)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const wantJSON = `{"mcp_grant":"all","builtin_declared":["apply_manifest"]}`
	if string(blob) != wantJSON {
		t.Errorf("wire form\n got: %s\nwant: %s\n"+
			"the key must not read as \"builtin\": mast installs no built-in tools on a specialist", blob, wantJSON)
	}
}

// Every member of a shipped roster answers the question, including the
// ones that declare no tools: block — "all" is a grant, and a field
// left empty there would read as "none".
func TestEveryCatalogEntryStatesItsMCPGrant(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "workloads", "gke-triage")
	built, err := buildRoot(context.Background(), discardLogger(),
		mastagent.NewEchoModel("echo"), "", "echo", dir, workload.DispatchCoordinator, nil)
	if err != nil {
		t.Fatalf("buildRoot: %v", err)
	}

	for _, e := range subagentCatalog(built.bundle, built.specs, built.dispatch) {
		switch e.Tools.MCPGrant {
		case attach.MCPGrantAll, attach.MCPGrantNone, attach.MCPGrantListed:
		default:
			t.Errorf("%s: mcp_grant = %q, want one of all/none/listed", e.Name, e.Tools.MCPGrant)
		}
	}
}

// A spec with no `model:` override inherits the root's, and the field
// stays empty rather than repeating the root model — a client showing
// "gemini-2.5-flash" against a specialist that declared nothing would
// be reporting a coincidence as a decision.
func TestSubagentCatalogLeavesInheritedModelEmpty(t *testing.T) {
	got := subagentCatalog(nil, []specialists.Spec{spec("log-analyst", specialists.ModeTask)}, workload.DispatchCoordinator)
	if got[0].Model != "" {
		t.Errorf("model = %q, want empty for a spec that declares no override", got[0].Model)
	}
}

// End to end against a shipped bundle: the roster the daemon actually
// loads projects with every member named and routed. The unit cases
// above build Specs by hand; this one proves the wiring reads the same
// roster buildRoot composed with.
func TestSubagentCatalogFromShippedWorkload(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "workloads", "gke-triage")
	built, err := buildRoot(context.Background(), discardLogger(),
		mastagent.NewEchoModel("echo"), "", "echo", dir, workload.DispatchCoordinator, nil)
	if err != nil {
		t.Fatalf("buildRoot: %v", err)
	}

	got := subagentCatalog(built.bundle, built.specs, built.dispatch)
	if len(got) != len(built.specs) || len(got) == 0 {
		t.Fatalf("catalog has %d entries for a %d-specialist roster", len(got), len(built.specs))
	}
	for i, e := range got {
		if e.Name == "" || e.Description == "" {
			t.Errorf("entry %d is unidentifiable: %+v", i, e)
		}
		if e.Invocation != attach.InvocationTransfer {
			t.Errorf("%s: invocation = %q, want %q under coordinator dispatch", e.Name, e.Invocation, attach.InvocationTransfer)
		}
		if e.Root == "" {
			t.Errorf("%s: no root; an operator cannot find the spec file", e.Name)
		}
		if e.Capability == "" {
			t.Errorf("%s: no capability; the roster's read/write split is invisible", e.Name)
		}
	}
}
