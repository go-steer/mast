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

// The fence on the collection leg (v0.5 W4.2). The exception it guards
// is the inverse of the precondition read's: that one is bounded by
// classification and can only widen towards safer calls, this one
// permits a MUTATING call because it is mast's own. So the bound is
// reachability — a collect tool must be reachable by nobody else.

func monitorBundle() workload.Bundle {
	yes := true
	return workload.Bundle{
		Name: "watch",
		ToolCatalog: workload.ToolCatalog{
			MCP: []workload.MCPServerRef{{Server: "gke"}},
			Tools: []workload.ToolPolicy{
				{Name: "k8s_findings_diff", Mutating: &yes},
			},
		},
		EdgeTrigger: workload.EdgeTrigger{
			Scheduled: &workload.ScheduledTrigger{Interval: "15m"},
		},
		Monitor: workload.Monitor{Collect: []workload.MonitorCollect{
			{Tool: "k8s_cluster_health", As: "health"},
			{Tool: "k8s_findings_diff", As: "transitions"},
		}},
	}
}

// enumeratedRoster reaches nothing the collection leg claims, which is
// the shape every monitoring workload with a tool-holding specialist has
// to be written in.
func enumeratedRoster() []specialists.Spec {
	return []specialists.Spec{{
		Name:        "diagnoser",
		Instruction: "diagnose",
		Mode:        specialists.ModeTask,
		Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{
			{Server: "gke", Tools: []string{"get_k8s_resource", "get_k8s_logs"}},
		}},
	}}
}

// Case 1: the obvious one. A specialist names the collect tool outright.
func TestMonitorRefusesASpecialistThatNamesACollectTool(t *testing.T) {
	specs := enumeratedRoster()
	specs[0].Tools.MCP[0].Tools = append(specs[0].Tools.MCP[0].Tools, "k8s_findings_diff")

	err := CheckMonitorCollectSurface(monitorBundle(), specs)
	if err == nil {
		t.Fatal("a roster holding a collect tool was accepted; the ungated door and the gated door lead to the same tool")
	}
	for _, want := range []string{"diagnoser", "k8s_findings_diff", "gke"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	// Both ways out, because which one is right depends on what the
	// operator meant and mast cannot tell.
	if !strings.Contains(err.Error(), "monitor.collect") || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("the refusal names no way out: %v", err)
	}
}

// Case 1, the other half of the surface: a built-in rather than an MCP
// tool. Nothing in mast's built-in five is a plausible collect tool
// today, which is precisely why an unchecked branch here would go
// unnoticed until something was.
func TestMonitorRefusesACollectToolInTheBuiltinAllowlist(t *testing.T) {
	specs := enumeratedRoster()
	specs[0].Tools.Builtin = []string{"k8s_cluster_health"}

	err := CheckMonitorCollectSurface(monitorBundle(), specs)
	if err == nil {
		t.Fatal("a built-in allowlist naming a collect tool was accepted")
	}
	if !strings.Contains(err.Error(), "built-in") || !strings.Contains(err.Error(), "k8s_cluster_health") {
		t.Errorf("refusal = %v, want it to name the built-in", err)
	}
}

// Case 2: a server allowlist with no tools: list grants every tool on
// that server, present and future. Refused whether or not a collect tool
// is on it today, because mast cannot tell without connecting — and this
// is the case CheckCapabilitySplit exempts a declared change executor
// from, which is why it has to be repeated here.
func TestMonitorRefusesAWholeServerGrant(t *testing.T) {
	specs := []specialists.Spec{{
		Name:        "executor",
		Instruction: "remediate",
		Mode:        specialists.ModeTask,
		Capability:  specialists.CapabilityChangeExecutor,
		Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{
			{Server: "gke"},
		}},
	}}

	err := CheckMonitorCollectSurface(monitorBundle(), specs)
	if err == nil {
		t.Fatal("a whole-server grant was accepted; a declared change executor holds every tool on gke, diff included")
	}
	if !strings.Contains(err.Error(), "no tools: list") {
		t.Errorf("refusal = %v, want it to name the un-enumerated grant", err)
	}
}

// TestMonitorRefusesAWholeServerGrantOnAnUnrelatedServer: same refusal
// even when the server is not the one the collect tools live on, because
// "the one they live on" is not a fact a YAML parse has. Refuse rather
// than guess — the same property the read this generalizes has.
func TestMonitorRefusesAWholeServerGrantOnAnUnrelatedServer(t *testing.T) {
	b := monitorBundle()
	b.ToolCatalog.MCP = append(b.ToolCatalog.MCP, workload.MCPServerRef{Server: "github"})
	specs := []specialists.Spec{{
		Name:        "reporter",
		Instruction: "report",
		Mode:        specialists.ModeTask,
		Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{
			{Server: "github"},
		}},
	}}

	if err := CheckMonitorCollectSurface(b, specs); err == nil {
		t.Fatal("a whole-server grant on another server was accepted; mast cannot know what lives on it")
	}
}

// Case 3: no tools.mcp key at all, which inherits the workload's whole
// catalog. The quietest of the three — the specialist file says nothing,
// so nothing in it looks like a grant.
func TestMonitorRefusesAnInheritAllRoster(t *testing.T) {
	specs := []specialists.Spec{{
		Name:        "diagnoser",
		Instruction: "diagnose",
		Mode:        specialists.ModeTask,
	}}

	err := CheckMonitorCollectSurface(monitorBundle(), specs)
	if err == nil {
		t.Fatal("a specialist with no tools.mcp allowlist was accepted; it inherits the whole catalog including the diff")
	}
	if !strings.Contains(err.Error(), "mcp: []") {
		t.Errorf("refusal = %v, want it to spell the deny-all escape", err)
	}
}

// The inherit-all case only bites when there is a catalog to inherit.
// A workload with no tool_catalog.mcp wires no MCP at all, so the
// specialist's silence grants nothing.
func TestMonitorInheritAllIsFineWithNoCatalog(t *testing.T) {
	b := monitorBundle()
	b.ToolCatalog.MCP = nil
	specs := []specialists.Spec{{Name: "d", Instruction: "diagnose", Mode: specialists.ModeTask}}

	if err := CheckMonitorCollectSurface(b, specs); err != nil {
		t.Errorf("refused a roster with no catalog to inherit: %v", err)
	}
}

// SingleTurn is exempt, as it is in CheckCapabilitySplit and for the
// same reason: BuildRoot builds those without toolsets, so they reach no
// tool of any class. That exemption is what lets the clearest
// demonstration of the whole idea exist — a bounded monitoring workload
// whose model holds nothing and still gets the transitions.
func TestMonitorExemptsSingleTurn(t *testing.T) {
	specs := []specialists.Spec{{
		Name:        "summarizer",
		Instruction: "summarize",
		Mode:        specialists.ModeSingleTurn,
		// Names a collect tool outright, and is still fine: this roster
		// cannot call it.
		Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{
			{Server: "gke", Tools: []string{"k8s_findings_diff"}},
		}},
	}}

	if err := CheckMonitorCollectSurface(monitorBundle(), specs); err != nil {
		t.Errorf("a SingleTurn roster was refused, but BuildRoot gives it no toolsets: %v", err)
	}
}

// The overwhelming majority of bundles: no monitor block, so no fence to
// apply. A check that taxed them would be a check nobody could adopt.
func TestMonitorNoBlockIsANoOp(t *testing.T) {
	b := monitorBundle()
	b.Monitor = workload.Monitor{}
	specs := []specialists.Spec{{Name: "d", Instruction: "diagnose", Mode: specialists.ModeTask}}

	if err := CheckMonitorCollectSurface(b, specs); err != nil {
		t.Errorf("a bundle with no monitor block was refused: %v", err)
	}
}

// The roster the fence is written for has to pass, or the fence is a ban
// on monitoring workloads that also diagnose.
func TestMonitorAcceptsAnEnumeratedRoster(t *testing.T) {
	if err := CheckMonitorCollectSurface(monitorBundle(), enumeratedRoster()); err != nil {
		t.Errorf("an enumerated roster that reaches no collect tool was refused: %v", err)
	}
}

// Both doors, the same pairing TestBothDoorsRefuse pins for the write
// surface. CheckRoster is the one the binary runs BEFORE wiring MCP, so
// an operator without credentials reads the real reason instead of a
// 403; BuildRoot is the library path, where there is no binary to have
// checked first.
// The roster is a declared change executor with a whole-server grant
// deliberately: that is the one shape CheckCapabilitySplit lets through,
// so whichever door refuses here, it is this check that did it.
func TestMonitorBothDoorsRefuse(t *testing.T) {
	b := monitorBundle()
	specs := []specialists.Spec{{
		Name:        "executor",
		Instruction: "remediate",
		Mode:        specialists.ModeTask,
		Capability:  specialists.CapabilityChangeExecutor,
		Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{
			{Server: "gke"},
		}},
	}}

	// Named, not just non-nil: BuildRoot has plenty of other ways to
	// fail, and a test satisfied by any of them would keep passing after
	// the check it is about was deleted.
	const want = "runs k8s_cluster_health, k8s_findings_diff on its own behalf"

	err := CheckRoster(b, specs, DispatchAuto)
	if err == nil {
		t.Fatal("CheckRoster accepted it; the binary would wire MCP before finding out")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("CheckRoster refused for another reason: %v", err)
	}

	_, _, err = BuildRoot(context.Background(), RootConfig{
		Bundle:    b,
		Specs:     specs,
		Model:     mastagent.NewEchoModel("echo"),
		ModelName: "echo",
	})
	if err == nil {
		t.Fatal("BuildRoot built the root; a library embedder gets no refusal at all")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("BuildRoot refused for another reason: %v", err)
	}
}

// The ack tool rides the same fence (v0.5 W4.6), and it matters more
// here than for collection. A collect tool the model could reach lets it
// gather facts ungated; an ack tool it could reach lets it SUPPRESS ITS
// OWN ALERTS — and the producer's triage write is typically not
// permission-gated on its side, so this check is the only thing in front
// of it. "There is no ack tool the model can call" has to be a property
// of the build, not a rule in a prompt.

func ackBundle() workload.Bundle {
	b := monitorBundle()
	b.Monitor.Ack = &workload.MonitorAck{Tool: "k8s_findings_ack"}
	return b
}

func TestMonitorRefusesASpecialistThatNamesTheAckTool(t *testing.T) {
	specs := enumeratedRoster()
	specs[0].Tools.MCP[0].Tools = append(specs[0].Tools.MCP[0].Tools, "k8s_findings_ack")

	err := CheckMonitorCollectSurface(ackBundle(), specs)
	if err == nil {
		t.Fatal("a roster holding the ack tool was accepted; the model can silence its own alerts")
	}
	if !strings.Contains(err.Error(), "k8s_findings_ack") {
		t.Errorf("refusal does not name the ack tool: %v", err)
	}
	// The remedy has to point at the line the operator wrote. An
	// operator who declared monitor.ack and is told to edit
	// monitor.collect goes looking for a block that is not there.
	if !strings.Contains(err.Error(), "monitor.ack") {
		t.Errorf("refusal = %v, want the remedy to name monitor.ack", err)
	}
	if strings.Contains(err.Error(), "monitor.collect") {
		t.Errorf("refusal = %v, want it not to blame monitor.collect for an ack tool", err)
	}
}

// A roster that reaches both gets both blocks named, once each: the
// operator has two lines to edit and the message says so.
func TestMonitorNamesBothBlocksWhenARosterReachesBoth(t *testing.T) {
	specs := enumeratedRoster()
	specs[0].Tools.MCP[0].Tools = append(specs[0].Tools.MCP[0].Tools, "k8s_findings_ack", "k8s_findings_diff")

	err := CheckMonitorCollectSurface(ackBundle(), specs)
	if err == nil {
		t.Fatal("a roster reaching both the collect tool and the ack tool was accepted")
	}
	if !strings.Contains(err.Error(), "monitor.ack / monitor.collect") {
		t.Errorf("refusal = %v, want both blocks named", err)
	}
}

// The shape validateMonitorAck permits and this check must still fence:
// an ack block with no collection at all. Monitor.Enabled() is false
// here, so a check that early-returned on it would leave the ack tool
// reachable by every roster in the workload.
func TestMonitorFencesAnAckOnlyWorkload(t *testing.T) {
	b := monitorBundle()
	b.Monitor = workload.Monitor{Ack: &workload.MonitorAck{Tool: "k8s_findings_ack"}}
	specs := enumeratedRoster()
	specs[0].Tools.MCP[0].Tools = append(specs[0].Tools.MCP[0].Tools, "k8s_findings_ack")

	if err := CheckMonitorCollectSurface(b, specs); err == nil {
		t.Fatal("an ack-only workload was not fenced; Monitor.Enabled() is false but the tool is still mast's to run")
	}
}

// And the roster the fence is written for still passes with an ack block
// present, or declaring one bans diagnosis.
func TestMonitorAcceptsAnEnumeratedRosterWithAnAck(t *testing.T) {
	if err := CheckMonitorCollectSurface(ackBundle(), enumeratedRoster()); err != nil {
		t.Errorf("an enumerated roster that reaches neither the collect tools nor the ack tool was refused: %v", err)
	}
}
