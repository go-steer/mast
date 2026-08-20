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

package toolcatalog_test

import (
	"context"
	"testing"

	"github.com/go-steer/mast/internal/toolcatalog"
	"github.com/go-steer/mast/pkg/planner"
)

// The provider tests below this package assert that every catalogued
// tool survives conversion. That invariant is only as strong as the
// catalog: if a rig stopped producing tools, or produced only the
// typed-Parameters ones, every provider test would go green while
// covering nothing. These tests are the floor under that.

func build(t *testing.T) []toolcatalog.Entry {
	t.Helper()
	endpoint, stop := toolcatalog.StartStubMCP()
	t.Cleanup(stop)

	cat, err := toolcatalog.Build(context.Background(), toolcatalog.Config{MCPEndpoint: endpoint})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return cat
}

// TestBuild_CoversEveryConstructionPath is the anti-vacuity check. The
// three spellings named here are the three ways a tool declaration
// reaches a provider adapter, and #154 was one of them going
// unvisited — so a catalog missing one is a catalog that would not
// have caught #154.
func TestBuild_CoversEveryConstructionPath(t *testing.T) {
	t.Parallel()
	cat := build(t)

	var typed, jsonSchema, mcpTools, plannerTools int
	for _, e := range cat {
		switch e.Spelling {
		case "Parameters":
			typed++
		case "ParametersJsonSchema":
			jsonSchema++
		default:
			t.Errorf("%s: no schema at all (spelling=%q) — a tool with no declared arguments is possible, but nothing in these rigs should have one; check what changed", e.Name, e.Spelling)
		}
		switch e.Rig {
		case "coordinator+mcp":
			if e.Name == "stub_search" || e.Name == "stub_apply_patch" {
				mcpTools++
			}
		case "planner":
			plannerTools++
		}
	}

	if typed == 0 {
		t.Errorf("no typed-Parameters tool in the catalog; the branch that always worked is now untested\n%s", toolcatalog.Summary(cat))
	}
	if jsonSchema == 0 {
		t.Errorf("no ParametersJsonSchema tool in the catalog; this is the branch #154 broke\n%s", toolcatalog.Summary(cat))
	}
	if mcpTools != 2 {
		t.Errorf("got %d MCP tools, want 2; the pkg/mcp construction path is not in the catalog\n%s", mcpTools, toolcatalog.Summary(cat))
	}
	if plannerTools == 0 {
		t.Errorf("the planner rig contributed nothing\n%s", toolcatalog.Summary(cat))
	}
}

// TestBuild_ContainsThePlannerVocabulary pins the tools by name. The
// count check above would still pass if the planner offered one tool
// instead of five; these are the names a workload's model actually
// needs to see, including pause_session, which only appears when a
// PauseRecorder is configured and is therefore the easiest one for a
// rig to lose by accident.
func TestBuild_ContainsThePlannerVocabulary(t *testing.T) {
	t.Parallel()
	cat := build(t)

	have := map[string]toolcatalog.Entry{}
	for _, e := range cat {
		have[e.Name] = e
	}
	for _, name := range []string{
		planner.ToolInvokeSpecialist,
		planner.ToolRunShapeLLMRouter,
		planner.ToolRunShapeFanOutFanIn,
		planner.ToolRequestOperatorInput,
		planner.ToolPauseSession,
		"finish_task",
		"invoke_remote_agent",
		"stub_search",
		"stub_apply_patch",
	} {
		e, ok := have[name]
		if !ok {
			t.Errorf("%q is missing from the catalog\n%s", name, toolcatalog.Summary(cat))
			continue
		}
		if !e.HasParams() {
			t.Errorf("%q is in the catalog with no arguments; every tool here takes some, so its declaration was read wrong", name)
		}
	}
}

// TestBuild_DerivesRequiredFromTheDeclaration guards the property that
// makes the catalog maintenance-free: expectations are read out of the
// declaration, never written down. A required argument that is not
// also a declared property would mean the derivation is reading two
// unrelated places.
func TestBuild_DerivesRequiredFromTheDeclaration(t *testing.T) {
	t.Parallel()
	for _, e := range build(t) {
		props := map[string]struct{}{}
		for _, p := range e.Props {
			props[p] = struct{}{}
		}
		for _, r := range e.Required {
			if _, ok := props[r]; !ok {
				t.Errorf("%s: %q is required but not a declared property (props=%v)", e.Name, r, e.Props)
			}
		}
	}
}

// TestBuild_RefusesAnEmptyMCPEndpoint keeps the MCP path from being
// skipped by omission. Defaulting to "no MCP server configured, carry
// on" would let a caller assemble a catalog that silently drops a
// third of its coverage.
func TestBuild_RefusesAnEmptyMCPEndpoint(t *testing.T) {
	t.Parallel()
	if _, err := toolcatalog.Build(context.Background(), toolcatalog.Config{}); err == nil {
		t.Fatal("Build with no MCPEndpoint returned nil error; the MCP construction path would be silently absent")
	}
}
