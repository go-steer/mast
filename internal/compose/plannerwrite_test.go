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
	"strings"
	"testing"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/graph"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// executorRoster is the shape #235 is about: a specialist that declares
// its write surface out loud, which is the only way to hold a mutating
// tool at all after CheckCapabilitySplit.
func executorRoster() []specialists.Spec {
	return []specialists.Spec{{
		Name:        "change-executor",
		Instruction: "remediate",
		Mode:        specialists.ModeTask,
		Capability:  specialists.CapabilityChangeExecutor,
		Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{
			{Server: "gke", Tools: []string{"scale_deployment"}},
		}},
	}}
}

func plannerBundle(onMutation workload.OnMutation) workload.Bundle {
	yes := true
	no := false
	return workload.Bundle{
		Name: "triage",
		ToolCatalog: workload.ToolCatalog{
			MCP: []workload.MCPServerRef{{Server: "gke"}},
			Tools: []workload.ToolPolicy{
				{Name: "get_k8s_resource", Mutating: &no},
				{Name: "scale_deployment", Mutating: &yes},
			},
		},
		HITL:    workload.HITL{OnMutation: onMutation},
		Planner: workload.Planner{Enabled: true},
	}
}

// The refusal itself. A planner dispatch runs its specialist on a runner
// built without the write gate, so a bundle that asked for approval on
// mutation would not get it — and the operator has no way to find that
// out except by watching an unapproved write land.
func TestPlannerRefusesAChangeExecutorWhenMutationIsGated(t *testing.T) {
	for _, policy := range []workload.OnMutation{
		workload.OnMutationRequireApproval,
		workload.OnMutationDryRun,
	} {
		t.Run(string(policy), func(t *testing.T) {
			err := CheckPlannerWriteSurface(plannerBundle(policy), executorRoster())
			if err == nil {
				t.Fatalf("hitl.on_mutation: %s + a planner-dispatched change executor was accepted; the write gate cannot reach it", policy)
			}
			for _, want := range []string{"change-executor", "#235", string(policy)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not mention %q: %v", want, err)
				}
			}
			// An operator's next move has to be in the message: the same
			// roster is fine under a shape whose runner carries the gate.
			// TestTheRefusalNamesEveryWayOut has the full list.
			if !strings.Contains(err.Error(), escapes[0]) {
				t.Errorf("the refusal names no way out: %v", err)
			}
		})
	}
}

// dry_run is the sharper half and the reason the check is not scoped to
// require_approval alone: its whole promise is that nothing changes, and
// a dispatched executor's call executes for real.
func TestDryRunIsRefusedNotJustApproval(t *testing.T) {
	err := CheckPlannerWriteSurface(plannerBundle(workload.OnMutationDryRun), executorRoster())
	if err == nil {
		t.Fatal("dry_run + planner dispatch was accepted; a mutating call inside the dispatch would execute for real")
	}
}

// Scoped to the promise actually broken. Under `apply` the gate was
// never going to stop the call, so the dispatch changes nothing about
// what executes — only the outbox record is missing, which is named in
// the docs rather than refused at startup.
func TestApplyIsExempt(t *testing.T) {
	if err := CheckPlannerWriteSurface(plannerBundle(workload.OnMutationApply), executorRoster()); err != nil {
		t.Fatalf("apply was refused, but there is no gate under apply for a dispatch to bypass: %v", err)
	}
}

// Two ways to be uninvolved, and both have to pass or the check is a tax
// on rosters that were never at risk.
func TestUninvolvedRostersAreUntouched(t *testing.T) {
	gated := plannerBundle(workload.OnMutationRequireApproval)

	if err := CheckPlannerWriteSurface(gated, plannerSpecs()); err != nil {
		t.Errorf("a read-only planner roster was refused: %v", err)
	}

	noPlanner := gated
	noPlanner.Planner = workload.Planner{}
	if err := CheckPlannerWriteSurface(noPlanner, executorRoster()); err != nil {
		t.Errorf("a change executor was refused under a non-planner shape, where the gate does reach it: %v", err)
	}
}

// escapes are the three ways out the refusal names, spelled exactly as
// an operator would read them off the message. Named here so the two
// tests below cannot drift apart: one checks the message says them, the
// other checks each one composes.
var escapes = []string{"coordinator", "graph", "apply"}

// The refusal is the design now, not containment waiting on a fix
// (v0.6 W9.1), which changes what its message owes. A stopgap can
// afford to say "not yet"; an answer has to say what to do instead, and
// every operator who hits this reads exactly these three words.
func TestTheRefusalNamesEveryWayOut(t *testing.T) {
	err := CheckPlannerWriteSurface(plannerBundle(workload.OnMutationRequireApproval), executorRoster())
	if err == nil {
		t.Fatal("no refusal to read")
	}
	for _, want := range escapes {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q as a way out: %v", want, err)
		}
	}
	// The promise this check was born with — that it comes out when the
	// boundary question settles — is withdrawn. It settled the other
	// way, so a message still hedging would be telling operators to
	// wait for something that is not coming.
	for _, stale := range []string{"not yet", "for now", "temporar", "until #235", "will be"} {
		if strings.Contains(err.Error(), stale) {
			t.Errorf("the refusal still reads as containment (%q): %v", stale, err)
		}
	}
}

// The exit criterion for W9.2, and the reason a doc-comment assertion
// would not have been one: a message that names a way out nobody
// checked is worse than a message that names none, because the operator
// spends their next hour on it. Each escape is composed here through
// the same door the binary uses.
func TestEveryEscapeTheRefusalNamesActuallyComposes(t *testing.T) {
	gated := plannerBundle(workload.OnMutationRequireApproval)

	// The two dispatch shapes: the roster is unchanged and the planner
	// is off, which is all "run this roster under coordinator" means.
	// These runners carry the gate, so the write is gated for real
	// rather than merely permitted to start.
	for _, tc := range []struct {
		escape   string
		dispatch Dispatch
		specs    []specialists.Spec
	}{
		{"coordinator", DispatchCoordinator, executorRoster()},
		{"graph", DispatchGraph, graphExecutorRoster()},
	} {
		t.Run(tc.escape, func(t *testing.T) {
			b := gated
			b.Planner = workload.Planner{}

			if err := CheckRoster(b, tc.specs, tc.dispatch); err != nil {
				t.Fatalf("the refusal sends operators to %s, and CheckRoster refuses it: %v", tc.escape, err)
			}
			if _, _, err := BuildRoot(context.Background(), RootConfig{
				Bundle:    b,
				Specs:     tc.specs,
				Model:     mastagent.NewEchoModel("echo"),
				ModelName: "echo",
				Dispatch:  tc.dispatch,
			}); err != nil {
				t.Fatalf("the refusal sends operators to %s, and BuildRoot refuses it: %v", tc.escape, err)
			}
		})
	}

	// The third escape keeps the planner and drops the gate, so it is
	// the one that changes what the operator gets rather than where
	// they run. It has to compose for the same reason.
	t.Run("apply", func(t *testing.T) {
		b := plannerBundle(workload.OnMutationApply)
		if err := CheckRoster(b, executorRoster(), DispatchAuto); err != nil {
			t.Fatalf("the refusal offers on_mutation: apply, and CheckRoster refuses it: %v", err)
		}
		if _, _, err := BuildRoot(context.Background(), RootConfig{
			Bundle:    b,
			Specs:     executorRoster(),
			Model:     mastagent.NewEchoModel("echo"),
			ModelName: "echo",
		}); err != nil {
			t.Fatalf("the refusal offers on_mutation: apply, and BuildRoot refuses it: %v", err)
		}
	})
}

// graphExecutorRoster is executorRoster() made routable: RosterShape
// reads graph off the classifier/_fallback pair, and a graph roster
// without both is a coordinator wearing the wrong label.
func graphExecutorRoster() []specialists.Spec {
	return append(executorRoster(),
		specialists.Spec{
			Name: "triage-classifier",
			Mode: specialists.ModeSingleTurn,
		},
		specialists.Spec{
			Name:        graph.FallbackName,
			Instruction: "look",
			Mode:        specialists.ModeTask,
			Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{
				{Server: "gke", Tools: []string{"get_k8s_resource"}},
			}},
		},
	)
}

// The refusal has to bite on both doors. BuildRoot is the library path;
// CheckRoster is the one the binary runs *before* wiring MCP, so that an
// operator without credentials reads the real reason instead of a 403.
func TestBothDoorsRefuse(t *testing.T) {
	b, specs := plannerBundle(workload.OnMutationRequireApproval), executorRoster()

	if err := CheckRoster(b, specs, DispatchAuto); err == nil {
		t.Error("CheckRoster accepted it; the binary would wire MCP before finding out")
	}

	_, _, err := BuildRoot(context.Background(), RootConfig{
		Bundle:    b,
		Specs:     specs,
		Model:     mastagent.NewEchoModel("echo"),
		ModelName: "echo",
	})
	if err == nil {
		t.Error("BuildRoot built the root; a library embedder gets no refusal at all")
	}
}
