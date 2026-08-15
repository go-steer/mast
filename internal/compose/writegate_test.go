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
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	adkagent "google.golang.org/adk/v2/agent"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// The gate's behaviour is pinned in pkg/approval. What is pinned here is
// the bundle-to-gate translation, which is where a workload's policy
// either reaches the tool-execution seam or silently does not.

func TestWriteGate_NoBundleNoGate(t *testing.T) {
	p, err := WriteGate(WriteGateConfig{})
	if err != nil {
		t.Fatalf("WriteGate: %v", err)
	}
	if p != nil {
		t.Fatalf("WriteGate returned a plugin with no bundle — a mutating call in a library embed would park with no resume surface to un-park it, which hangs the caller rather than protecting them")
	}
}

// TestWriteGate_BundleDefaultsToGated is the safety default arriving
// where it matters: a bundle that says nothing about mutation still gets
// a gate registered.
func TestWriteGate_BundleDefaultsToGated(t *testing.T) {
	p, err := WriteGate(WriteGateConfig{Bundle: &workload.Bundle{Name: "b"}})
	if err != nil {
		t.Fatalf("WriteGate: %v", err)
	}
	if p == nil {
		t.Fatalf("WriteGate returned no plugin for a bundle with no hitl block; the default is require_approval and it must be enforced, not just documented")
	}
	if got, want := p.Name(), approval.PluginName; got != want {
		t.Errorf("plugin name = %q, want %q", got, want)
	}
	if p.BeforeToolCallback() == nil {
		t.Error("plugin has no BeforeToolCallback; nothing would intercept a tool call")
	}
}

// TestWriteGate_DefaultGateIsSuppliedUnderRequireApproval: approval.New
// refuses require_approval without a gate, so WriteGate must supply one
// rather than pass the refusal up as a startup failure.
func TestWriteGate_DefaultGateIsSuppliedUnderRequireApproval(t *testing.T) {
	p, err := WriteGate(WriteGateConfig{Bundle: &workload.Bundle{
		Name: "b",
		HITL: workload.HITL{OnMutation: workload.OnMutationRequireApproval},
	}})
	if err != nil {
		t.Fatalf("WriteGate: %v", err)
	}
	if p == nil {
		t.Fatal("WriteGate returned no plugin under an explicit require_approval")
	}
}

func TestWriteGate_PolicyPassesThrough(t *testing.T) {
	for _, policy := range []workload.OnMutation{
		workload.OnMutationApply,
		workload.OnMutationDryRun,
	} {
		p, err := WriteGate(WriteGateConfig{Bundle: &workload.Bundle{
			Name: "b",
			HITL: workload.HITL{OnMutation: policy},
		}})
		if err != nil {
			t.Fatalf("WriteGate(%s): %v", policy, err)
		}
		// apply and dry_run both still register: apply audits every
		// mutating call it lets through, and dry_run has to intercept.
		if p == nil {
			t.Errorf("WriteGate(%s) returned no plugin", policy)
		}
	}
}

// TestWriteGate_UnknownPolicyIsAStartupError is the second half of the
// loader's enum check, for the paths that build a Bundle in code (the
// eval rig, a library embed) and never go through Load.
func TestWriteGate_UnknownPolicyIsAStartupError(t *testing.T) {
	_, err := WriteGate(WriteGateConfig{Bundle: &workload.Bundle{
		Name: "b",
		HITL: workload.HITL{OnMutation: workload.OnMutation("aply")},
	}})
	if err == nil {
		t.Fatalf("WriteGate accepted an unknown on_mutation; a typo would fall through to whatever the switch's default happened to be")
	}
	if !strings.Contains(err.Error(), "aply") {
		t.Errorf("error = %v, want it to quote the offending value", err)
	}
}

// TestWriteGate_SuppliedPredicateWins: the caller passes the predicate
// the effects outbox is using. If the gate derived its own instead, a
// tool could be recorded as an effect but never gated, or gated but
// never recorded.
func TestWriteGate_SuppliedPredicateWins(t *testing.T) {
	var asked []string
	pred := func(name string) effects.Class {
		asked = append(asked, name)
		return effects.ClassReadOnly
	}
	p, err := WriteGate(WriteGateConfig{
		Bundle:    &workload.Bundle{Name: "b"},
		Predicate: pred,
	})
	if err != nil {
		t.Fatalf("WriteGate: %v", err)
	}
	// The read-only branch of the gate's callback returns before it
	// touches the agent context, which is what lets this probe run
	// without a live invocation. Anything that reaches further — the
	// derived predicate calling an unknown tool mutating, say — hits the
	// nil context instead, so say what that means rather than leaving a
	// bare segfault.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the gate went past its read-only branch (%v), so the supplied predicate was not the one consulted", r)
		}
	}()
	out, err := p.BeforeToolCallback()(nil, stubTool("get_pods"), nil)
	if err != nil {
		t.Fatalf("BeforeToolCallback: %v", err)
	}
	if out != nil {
		t.Errorf("read-only tool got response %v, want nil (the tool runs)", out)
	}
	if len(asked) != 1 || asked[0] != "get_pods" {
		t.Errorf("predicate calls = %v, want exactly [get_pods] — the supplied predicate was not the one consulted", asked)
	}
}

// stubTool is the minimum tool.Tool the gate reads: its name.
type stubTool string

func (s stubTool) Name() string        { return string(s) }
func (s stubTool) Description() string { return "stub" }
func (s stubTool) IsLongRunning() bool { return false }

// The producer contract's half of the translation (v0.4 W7.0): which
// tools a proposed change may name, and what happens when the
// composition cannot answer that.

func execSpec(name string, tools ...string) specialists.Spec {
	s := specialists.Spec{Name: name, Capability: specialists.CapabilityChangeExecutor}
	s.Tools.MCP = []specialists.MCPAllowlist{{Server: "gke", Tools: tools}}
	return s
}

func diagSpec(name string, tools ...string) specialists.Spec {
	s := specialists.Spec{Name: name, Capability: specialists.CapabilityReadOnly}
	s.Tools.MCP = []specialists.MCPAllowlist{{Server: "gke", Tools: tools}}
	return s
}

// TestChangeSurfaceIsTheExecutorsAllowlist: a diagnoser may propose only
// what a change executor in the same roster could carry out. Proposing
// a tool no executor holds is a proposal that dies after the operator
// approves it, which is the worst place to find out.
func TestChangeSurfaceIsTheExecutorsAllowlist(t *testing.T) {
	specs := []specialists.Spec{
		diagSpec("workload-diagnoser", "get_k8s_resource"),
		execSpec("change-executor", "patch_k8s_resource", "apply_k8s_manifest"),
	}
	surface, exhaustive := changeSurface(specs)
	if !exhaustive {
		t.Fatal("a fully enumerated roster reported a non-exhaustive surface, so the contract fell back to accepting any wired tool")
	}
	if !surface["patch_k8s_resource"] || !surface["apply_k8s_manifest"] {
		t.Errorf("surface = %v, want the executor's two tools", sortedKeys(surface))
	}
	// The diagnoser's read tools are not remediation. A change set
	// naming get_k8s_resource is a specialist that misread the field.
	if surface["get_k8s_resource"] {
		t.Errorf("surface = %v, want no read-only tool from a non-executor spec", sortedKeys(surface))
	}
}

// TestChangeSurfaceUnenumeratedGrantsAreNotASurface: CheckCapabilitySplit
// lets a change executor inherit a whole server. No finite name list
// describes that, so the contract must say so rather than invent one —
// a surface built from an inherit-all executor would be empty, and an
// empty surface refuses everything.
func TestChangeSurfaceUnenumeratedGrantsAreNotASurface(t *testing.T) {
	inheritAll := specialists.Spec{Name: "e", Capability: specialists.CapabilityChangeExecutor}
	wholeServer := specialists.Spec{Name: "e", Capability: specialists.CapabilityChangeExecutor}
	wholeServer.Tools.MCP = []specialists.MCPAllowlist{{Server: "gke"}}

	for _, tc := range []struct {
		name  string
		specs []specialists.Spec
	}{
		{"no tools.mcp key at all", []specialists.Spec{inheritAll}},
		{"a server with no tools list", []specialists.Spec{wholeServer}},
		{"no change executor in the roster", []specialists.Spec{diagSpec("d", "get_k8s_resource")}},
		{"no roster at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, exhaustive := changeSurface(tc.specs); exhaustive {
				t.Error("reported an exhaustive surface it cannot know")
			}
		})
	}
}

// TestChangeSetCheckerNarrowsToTheSurface drives the built checker,
// because a surface computed and then not consulted is not enforcement.
func TestChangeSetCheckerNarrowsToTheSurface(t *testing.T) {
	c := changeSetChecker(WriteGateConfig{
		Bundle: &workload.Bundle{Name: "b"},
		Specs:  []specialists.Spec{execSpec("change-executor", "patch_k8s_resource")},
		ToolSchemas: func(string) (*jsonschema.Schema, error) {
			return &jsonschema.Schema{Type: "object"}, nil
		},
	})
	if !c.Declares("patch_k8s_resource") {
		t.Error("the executor's own tool was refused")
	}
	if c.Declares("delete_k8s_resource") {
		t.Error("a tool no executor in this roster holds was accepted; an operator would approve a change nothing can run")
	}
}

// TestChangeSetCheckerWithoutASurfaceStillNeedsARealTool: with no
// enumerable executor surface the contract falls back to the resolver,
// which is a weaker check but not no check.
func TestChangeSetCheckerWithoutASurfaceStillNeedsARealTool(t *testing.T) {
	c := changeSetChecker(WriteGateConfig{
		Bundle: &workload.Bundle{Name: "b"},
		ToolSchemas: func(name string) (*jsonschema.Schema, error) {
			if name != "patch_k8s_resource" {
				return nil, fmt.Errorf("no tool named %q is wired", name)
			}
			return &jsonschema.Schema{Type: "object"}, nil
		},
	})
	if !c.Declares("kubectl_scale") {
		t.Fatal("Declares narrowed with no surface to narrow to")
	}
	if _, err := c.Check([]approval.ProposedChange{{Tool: "kubectl_scale"}}); err == nil {
		t.Error("an invented tool passed a composition with no executor allowlist; the resolver is the last check there and it did not run")
	}
}

// TestChangeSetCheckerFailsClosedWithNoResolver is the fail-closed rule
// written down as behaviour: a deployment that cannot look up a tool's
// arguments refuses proposals rather than passing unverified ones to an
// operator.
func TestChangeSetCheckerFailsClosedWithNoResolver(t *testing.T) {
	c := changeSetChecker(WriteGateConfig{Bundle: &workload.Bundle{Name: "b"}})
	_, err := c.Check([]approval.ProposedChange{{Tool: "patch_k8s_resource"}})
	if err == nil {
		t.Fatal("a change was accepted by a deployment that cannot check its arguments")
	}
	if !strings.Contains(err.Error(), "patch_k8s_resource") {
		t.Errorf("refusal does not name the tool: %v", err)
	}
}

// TestWriteGate_InstallsTheProducerContract: the checker has to reach
// the plugin the daemon registers, not just exist in this package.
func TestWriteGate_InstallsTheProducerContract(t *testing.T) {
	var asked []string
	p, err := WriteGate(WriteGateConfig{
		Bundle: &workload.Bundle{Name: "b"},
		Specs:  []specialists.Spec{execSpec("change-executor", "patch_k8s_resource")},
		ToolSchemas: func(name string) (*jsonschema.Schema, error) {
			asked = append(asked, name)
			return &jsonschema.Schema{Type: "object"}, nil
		},
	})
	if err != nil {
		t.Fatalf("WriteGate: %v", err)
	}
	out, err := p.BeforeToolCallback()(nil, stubTool(approval.FinishTaskToolName), map[string]any{
		approval.ChangeSetField: []any{map[string]any{"tool": "kubectl_scale", "arguments": "{}"}},
	})
	if err != nil {
		t.Fatalf("BeforeToolCallback: %v", err)
	}
	if out == nil || out["error"] != "invalid_proposed_change" {
		t.Fatalf("finish_task response = %v, want the producer contract's refusal — the checker never reached the registered plugin", out)
	}
	if len(asked) != 0 {
		t.Errorf("the resolver was consulted for %v, but the catalog check should have refused first", asked)
	}
}

// The change-set grant half of the translation (v0.4 W7): what a bundle
// says about how long one operator answer lasts, and what each granted
// call is re-checked against before it fires.

// preconditionBundle is a workload where scaling declares a freshness
// check against a read, which is the shape W7 exists for.
func preconditionBundle(read string, readMutating bool) *workload.Bundle {
	writes := true
	return &workload.Bundle{
		Name: "b",
		ToolCatalog: workload.ToolCatalog{Tools: []workload.ToolPolicy{
			{
				Name:     "scale_deployment",
				Mutating: &writes,
				Precondition: &workload.Precondition{
					Read:     read,
					ArgsFrom: map[string]string{"name": "deployment"},
					Fields:   []string{"spec.replicas"},
				},
			},
			{Name: read, Mutating: &readMutating},
		}},
	}
}

// TestChangeSetGrants_MutatingReadRefusesToStart: a freshness check that
// is itself a mutation would change the cluster once at approval time
// and again before every granted call — unapproved, unrecorded, and in
// the name of safety. There is no version of that worth running, so it
// fails the daemon rather than warning.
func TestChangeSetGrants_MutatingReadRefusesToStart(t *testing.T) {
	_, err := WriteGate(WriteGateConfig{Bundle: preconditionBundle("restart_deployment", true)})
	if err == nil {
		t.Fatal("WriteGate started with a mutating precondition read; every granted call would silently restart the deployment it was checking")
	}
	for _, want := range []string{"scale_deployment", "restart_deployment", "mutating"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("startup error does not mention %q: %v", want, err)
		}
	}
}

// TestChangeSetGrants_UnclassifiedReadIsMutating: the predicate calls an
// unlisted tool mutating, so a precondition pointing at a tool the
// catalog never declares is refused too. That is the fail-closed default
// doing its job rather than an oversight — and the refusal says how to
// fix it, because "declare the read in tool_catalog" is not something an
// operator would guess from "is mutating".
func TestChangeSetGrants_UnclassifiedReadIsMutating(t *testing.T) {
	b := preconditionBundle("get_deployment", false)
	b.ToolCatalog.Tools = b.ToolCatalog.Tools[:1] // drop the read's own entry
	_, err := WriteGate(WriteGateConfig{Bundle: b})
	if err == nil {
		t.Fatal("WriteGate accepted a precondition reading a tool the catalog never classifies")
	}
	if !strings.Contains(err.Error(), "mutating: false") {
		t.Errorf("refusal does not say how to declare the read: %v", err)
	}
}

// TestChangeSetGrants_DeclarationsReachTheFreshnessRules: a precondition
// parsed out of the bundle and then not handed to the gate is a check
// that never runs.
func TestChangeSetGrants_DeclarationsReachTheFreshnessRules(t *testing.T) {
	b := preconditionBundle("get_deployment", false)
	b.HITL.ChangeSetTTL = "45m"
	var read []string
	f, err := changeSetGrants(WriteGateConfig{
		Bundle: b,
		ToolRead: func(_ adkagent.Context, name string, _ map[string]any) (map[string]any, error) {
			read = append(read, name)
			return map[string]any{"ok": true}, nil
		},
	}, MutationPredicate(*b, nil))
	if err != nil {
		t.Fatalf("changeSetGrants: %v", err)
	}
	if f.TTL != 45*time.Minute {
		t.Errorf("TTL = %v, want the bundle's 45m", f.TTL)
	}
	pre, err := f.Precondition("scale_deployment")
	if err != nil {
		t.Fatalf("Precondition: %v", err)
	}
	if pre == nil {
		t.Fatal("scale_deployment has no precondition at the gate, so its grants would be bounded by the clock alone")
	}
	if pre.Read != "get_deployment" || pre.ArgsFrom["name"] != "deployment" || len(pre.Fields) != 1 {
		t.Errorf("precondition = %+v, want the bundle's declaration carried across whole", pre)
	}
	// A tool that declares nothing gets nothing — not an empty
	// declaration, which would read as a check that always passes.
	other, err := f.Precondition("get_deployment")
	if err != nil || other != nil {
		t.Errorf("Precondition(get_deployment) = %+v, %v; want nil, nil", other, err)
	}
	if _, err := f.Read(nil, "get_deployment", nil); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(read) != 1 || read[0] != "get_deployment" {
		t.Errorf("reads = %v, want the supplied ToolRead to be the one wired in", read)
	}
}

// TestChangeSetGrants_BadTTLIsAStartupError is the loader's duration
// check reaching the paths that build a Bundle in code and never go
// through Load.
func TestChangeSetGrants_BadTTLIsAStartupError(t *testing.T) {
	b := &workload.Bundle{Name: "b"}
	b.HITL.ChangeSetTTL = "10 minutes"
	if _, err := WriteGate(WriteGateConfig{Bundle: b}); err == nil {
		t.Fatal("WriteGate started with an unparseable change_set_ttl, so the window would silently be the default")
	}
}

// TestChangeSetGrants_WarnsWhenItCannotRead: a deployment with no way to
// run a read still starts — the calls just park one at a time — but it
// must say so. The failure mode this closes is a bundle author writing
// preconditions, seeing a clean startup, and believing they are checked.
func TestChangeSetGrants_WarnsWhenItCannotRead(t *testing.T) {
	b := preconditionBundle("get_deployment", false)
	pred := MutationPredicate(*b, nil)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := changeSetGrants(WriteGateConfig{Bundle: b, Logger: logger}, pred); err != nil {
		t.Fatalf("changeSetGrants: %v", err)
	}
	if !strings.Contains(buf.String(), "cannot run a read on its own behalf") {
		t.Errorf("no warning that the declared preconditions will not be checked:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "scale_deployment") {
		t.Errorf("the warning does not name the affected tool:\n%s", buf.String())
	}

	buf.Reset()
	_, err := changeSetGrants(WriteGateConfig{
		Bundle: b, Logger: logger,
		ToolRead: func(adkagent.Context, string, map[string]any) (map[string]any, error) { return nil, nil },
	}, pred)
	if err != nil {
		t.Fatalf("changeSetGrants: %v", err)
	}
	if strings.Contains(buf.String(), "cannot run a read") {
		t.Errorf("warned about reads on a deployment that can run them:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "can_read=true") {
		t.Errorf("startup log does not record that preconditions are live:\n%s", buf.String())
	}
}

// TestChangeSetGrants_NoPreconditionsIsStillGrantable: a bundle that
// declares no freshness check still gets grants, bounded by the TTL.
// Turning them off there would mean `scope: change_set` was refused for
// every workload that had not opted into preconditions, which is not
// what the verdict schema promises.
func TestChangeSetGrants_NoPreconditionsIsStillGrantable(t *testing.T) {
	b := &workload.Bundle{Name: "b"}
	f, err := changeSetGrants(WriteGateConfig{Bundle: b}, MutationPredicate(*b, nil))
	if err != nil {
		t.Fatalf("changeSetGrants: %v", err)
	}
	if f == nil {
		t.Fatal("no freshness rules, so scope: change_set would be refused for every workload without preconditions")
	}
	pre, err := f.Precondition("scale_deployment")
	if err != nil || pre != nil {
		t.Errorf("Precondition = %+v, %v; want nil, nil", pre, err)
	}
}
