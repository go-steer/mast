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

package workload_test

import (
	"strings"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/workload"
)

// The bundle half of W7: what a workload declares so that an approved
// change set can be re-checked before its remaining calls fire.

func TestLoad_Precondition(t *testing.T) {
	path := writeBundle(t, "b.yaml", `name: x
specialists: [a]
tool_catalog:
  tools:
    - name: scale_deployment
      mutating: true
      precondition:
        read: get_deployment
        args: {output: json}
        args_from: {name: deployment, namespace: namespace}
        fields: [spec.replicas, metadata.generation]
    - name: get_deployment
      mutating: false
`)
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pre := b.ToolCatalog.Tools[0].Precondition
	if pre == nil {
		t.Fatal("scale_deployment has no precondition")
	}
	if pre.Read != "get_deployment" {
		t.Errorf("read = %q, want get_deployment", pre.Read)
	}
	if pre.Args["output"] != "json" {
		t.Errorf("args = %v, want the literal output:json", pre.Args)
	}
	if pre.ArgsFrom["name"] != "deployment" || pre.ArgsFrom["namespace"] != "namespace" {
		t.Errorf("args_from = %v, want the change's own arguments mapped onto the read's", pre.ArgsFrom)
	}
	if len(pre.Fields) != 2 || pre.Fields[0] != "spec.replicas" {
		t.Errorf("fields = %v, want the two declared paths in order", pre.Fields)
	}
	// Omitted is nil, not an empty declaration: a tool with no
	// precondition is bounded by the TTL alone, and mast says so in the
	// approval question rather than pretending to check nothing.
	if b.ToolCatalog.Tools[1].Precondition != nil {
		t.Errorf("get_deployment precondition = %+v, want nil", b.ToolCatalog.Tools[1].Precondition)
	}
}

// TestLoad_PreconditionErrors: what the bundle alone can catch. A
// declaration that cannot describe a real check must fail at load
// rather than at 03:00, when the answer to "is this still true" is
// what stands between an approval and a cluster.
func TestLoad_PreconditionErrors(t *testing.T) {
	for _, tc := range []struct{ name, body, wantErr string }{
		{
			"no read tool",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      precondition:\n        fields: [a]\n",
			"names no read tool",
		},
		{
			// A change cannot be its own precondition: running the write
			// to check whether the write is still safe is not a check.
			"reads itself",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      precondition:\n        read: t\n",
			"cannot be its own precondition",
		},
		{
			"empty field path",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      precondition:\n        read: r\n        fields: [\"\"]\n",
			"empty path",
		},
		{
			"args_from with an empty side",
			"name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n      precondition:\n        read: r\n        args_from: {name: \"\"}\n",
			"both sides name an argument",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeBundle(t, "b.yaml", tc.body)
			_, err := workload.Load(path)
			if err == nil {
				t.Fatalf("Load accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestHITL_ChangeSetTTL(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nhitl:\n  change_set_ttl: 45m\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := b.HITL.EffectiveChangeSetTTL()
	if err != nil {
		t.Fatalf("EffectiveChangeSetTTL: %v", err)
	}
	if got != 45*time.Minute {
		t.Errorf("EffectiveChangeSetTTL = %v, want 45m", got)
	}
}

// TestHITL_ChangeSetTTLUnsetIsZero: unset is "the write gate's default",
// resolved by the caller. The bundle does not carry the number, because
// the default belongs to the gate.
func TestHITL_ChangeSetTTLUnsetIsZero(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := b.HITL.EffectiveChangeSetTTL()
	if err != nil || got != 0 {
		t.Errorf("EffectiveChangeSetTTL = %v, %v; want 0, nil", got, err)
	}
}

// TestHITL_BadChangeSetTTLIsRefusedAtLoad: a typo'd duration silently
// becoming the default is how a workload ends up with a freshness
// window nobody chose.
func TestHITL_BadChangeSetTTLIsRefusedAtLoad(t *testing.T) {
	for _, bad := range []string{"10 minutes", "0s", "-5m", "ten"} {
		path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nhitl:\n  change_set_ttl: \""+bad+"\"\n")
		_, err := workload.Load(path)
		if err == nil {
			t.Errorf("Load accepted change_set_ttl: %q", bad)
			continue
		}
		if !strings.Contains(err.Error(), "change_set_ttl") {
			t.Errorf("error for %q = %v, want it to name the key", bad, err)
		}
	}
}

// TestHITL_ChangeSetTTLFoldsThroughTheAlias: hitl_policy: is the
// documented spelling, and foldHITLPolicy compares the two structs — a
// field that broke comparability would fail the build, and a field the
// fold forgot would silently drop the operator's window.
func TestHITL_ChangeSetTTLFoldsThroughTheAlias(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nhitl_policy:\n  change_set_ttl: 30s\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := b.HITL.EffectiveChangeSetTTL()
	if err != nil {
		t.Fatalf("EffectiveChangeSetTTL: %v", err)
	}
	if got != 30*time.Second {
		t.Errorf("EffectiveChangeSetTTL = %v, want 30s", got)
	}
}
