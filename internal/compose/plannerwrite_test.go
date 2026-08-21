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
			if !strings.Contains(err.Error(), "coordinator") {
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
