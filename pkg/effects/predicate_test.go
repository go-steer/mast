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

package effects

import (
	"testing"

	"github.com/go-steer/mast/pkg/planner"
)

func TestPredicateDefaults(t *testing.T) {
	pred := NewPredicate(nil)
	cases := []struct {
		name string
		want Class
	}{
		{"adk_request_input", ClassReadOnly},
		{"adk_request_credential", ClassReadOnly},
		{"adk_request_confirmation", ClassReadOnly},
		{"finish_task", ClassReadOnly},
		{"request_operator_input", ClassReadOnly},
		{"pause_session", ClassReadOnly}, // v0.2 plane-A self-pause: a park, not an effect
		{"invoke_specialist", ClassSpawning},
		{"run_shape_llm_router", ClassSpawning},
		{"run_shape_fan_out_fan_in", ClassSpawning},
		{"invoke_remote_agent", ClassMutating},
		{"gke_scale_deployment", ClassMutating}, // unknown MCP tool: default-deny
		{"list_clusters", ClassMutating},        // "obviously read-only" names get no free pass
	}
	for _, c := range cases {
		if got := pred(c.name); got != c.want {
			t.Errorf("pred(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPredicateOverrides(t *testing.T) {
	f, tr := false, true
	pred := NewPredicate(Overrides(nil, []ToolPolicy{
		{Name: "list_clusters", Mutating: &f},
		{Name: "request_operator_input", Mutating: &tr},
		{Name: "unnamed", Mutating: nil}, // no override value: ignored
		{Name: "", Mutating: &f},         // no name: ignored
	}))
	if got := pred("list_clusters"); got != ClassReadOnly {
		t.Errorf("override mutating:false → %v, want ClassReadOnly", got)
	}
	if got := pred("request_operator_input"); got != ClassMutating {
		t.Errorf("override mutating:true on a builtin → %v, want ClassMutating (override outranks builtin table)", got)
	}
	if got := pred("unnamed"); got != ClassMutating {
		t.Errorf("nil-valued override must not change the default: got %v", got)
	}
	// Control calls outrank overrides: un-gating the HITL surface (or
	// gating it) would corrupt pause/resume semantics.
	predCtl := NewPredicate(map[string]bool{"adk_request_input": true})
	if got := predCtl("adk_request_input"); got != ClassReadOnly {
		t.Errorf("control call with override → %v, want ClassReadOnly (control outranks)", got)
	}
}

// TestBuiltinNamesMatchRegistrations pins the string literals in
// builtinClasses to the constants the owning packages export — drift
// between the outbox's table and an actual tool registration must fail
// CI, not silently reclassify a tool to the default.
func TestBuiltinNamesMatchRegistrations(t *testing.T) {
	pins := map[string]string{
		planner.ToolInvokeSpecialist:     "invoke_specialist",
		planner.ToolRunShapeLLMRouter:    "run_shape_llm_router",
		planner.ToolRunShapeFanOutFanIn:  "run_shape_fan_out_fan_in",
		planner.ToolRequestOperatorInput: "request_operator_input",
	}
	for constant, literal := range pins {
		if constant != literal {
			t.Errorf("planner constant %q != outbox table literal %q", constant, literal)
		}
		if _, ok := builtinClasses[literal]; !ok {
			t.Errorf("builtinClasses is missing an entry for %q", literal)
		}
	}
	// pause_session is a CONTROL call (a park, not an effect), so its
	// pin checks the control table rather than builtinClasses.
	if planner.ToolPauseSession != "pause_session" {
		t.Errorf("planner.ToolPauseSession = %q, want the control-table literal \"pause_session\"", planner.ToolPauseSession)
	}
	if !controlCalls[planner.ToolPauseSession] {
		t.Errorf("controlCalls is missing %q", planner.ToolPauseSession)
	}
	// pkg/federation exports no name constant; the literal is pinned in
	// its registration (pkg/federation/tool.go). If this entry ever
	// goes stale the federation tool silently falls to the default
	// class — which is ClassMutating, the same value, so the failure
	// mode is benign; the entry documents intent.
	if builtinClasses["invoke_remote_agent"] != ClassMutating {
		t.Error("invoke_remote_agent must classify mutating (remote effects are invisible to this process)")
	}
}

// TestCheckNameCollisions is the gate finding N2 guard: a sub-agent name
// that also names a mutating/spawning tool is a fail-open durability hole
// (the delegation exclusion hides the genuine tool call), so it must be
// reported; a read-only or absent tool of the same name must not be.
// Neutralize the mutating/spawning filter in CheckNameCollisions (report
// every tool name) and the "read-only collision is silent" case fails;
// neutralize the subAgents intersection (report nothing) and every
// positive case fails.
func TestCheckNameCollisions(t *testing.T) {
	tr, f := true, false
	subAgents := map[string]bool{
		"triage_bot": true,
		"deploy":     true, // operator specialist sharing a tool verb
		"reader":     true, // collides only with a read-only tool
	}

	t.Run("declared mutating tool collision reported", func(t *testing.T) {
		pred := NewPredicate(Overrides(nil, []ToolPolicy{{Name: "deploy", Mutating: &tr}}))
		got := CheckNameCollisions(subAgents, pred, []ToolPolicy{{Name: "deploy", Mutating: &tr}})
		if len(got) != 1 || got[0] != "deploy" {
			t.Fatalf("CheckNameCollisions = %v, want [deploy]", got)
		}
	})

	t.Run("nil-override MCP tool defaults mutating and collides", func(t *testing.T) {
		// A declared tool with no class override defaults to mutating
		// (default-deny). A specialist sharing its name is still a hole.
		pred := NewPredicate(nil)
		got := CheckNameCollisions(subAgents, pred, []ToolPolicy{{Name: "deploy", Mutating: nil}})
		if len(got) != 1 || got[0] != "deploy" {
			t.Fatalf("CheckNameCollisions = %v, want [deploy] (nil override → mutating default)", got)
		}
	})

	t.Run("read-only tool collision is harmless and silent", func(t *testing.T) {
		pred := NewPredicate(Overrides(nil, []ToolPolicy{{Name: "reader", Mutating: &f}}))
		got := CheckNameCollisions(subAgents, pred, []ToolPolicy{{Name: "reader", Mutating: &f}})
		if len(got) != 0 {
			t.Fatalf("CheckNameCollisions = %v, want none (read-only tool never dangles)", got)
		}
	})

	t.Run("builtin spawning tool collision reported", func(t *testing.T) {
		// A specialist named after a builtin spawning tool (invoke_specialist)
		// collides even with no declared policies.
		subs := map[string]bool{"invoke_specialist": true}
		got := CheckNameCollisions(subs, NewPredicate(nil), nil)
		if len(got) != 1 || got[0] != "invoke_specialist" {
			t.Fatalf("CheckNameCollisions = %v, want [invoke_specialist]", got)
		}
	})

	t.Run("no collision when tool name is not a sub-agent", func(t *testing.T) {
		pred := NewPredicate(Overrides(nil, []ToolPolicy{{Name: "scale_up", Mutating: &tr}}))
		got := CheckNameCollisions(subAgents, pred, []ToolPolicy{{Name: "scale_up", Mutating: &tr}})
		if len(got) != 0 {
			t.Fatalf("CheckNameCollisions = %v, want none (scale_up is not a sub-agent)", got)
		}
	})

	t.Run("multiple collisions sorted", func(t *testing.T) {
		subs := map[string]bool{"deploy": true, "apply": true, "reader": true}
		policies := []ToolPolicy{
			{Name: "deploy", Mutating: &tr},
			{Name: "apply", Mutating: nil}, // default mutating
			{Name: "reader", Mutating: &f}, // read-only: excluded
		}
		// The predicate must be built from the same overrides the caller
		// passes as policies (cmd/mast wires them from one bundle).
		pred := NewPredicate(Overrides(nil, policies))
		got := CheckNameCollisions(subs, pred, policies)
		if len(got) != 2 || got[0] != "apply" || got[1] != "deploy" {
			t.Fatalf("CheckNameCollisions = %v, want [apply deploy] (sorted, read-only dropped)", got)
		}
	})

	t.Run("empty inputs", func(t *testing.T) {
		if got := CheckNameCollisions(nil, NewPredicate(nil), nil); got != nil {
			t.Fatalf("nil subAgents → %v, want nil", got)
		}
		if got := CheckNameCollisions(subAgents, nil, nil); got != nil {
			t.Fatalf("nil predicate → %v, want nil", got)
		}
	})
}
