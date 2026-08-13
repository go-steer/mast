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
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/effects"
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
