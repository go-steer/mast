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
	"context"
	"log/slog"
	"strings"
	"testing"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// triageCatalog is a workload that classifies its reads and leaves its
// writes to the default-deny predicate — the shape the shipped
// gke-triage bundle has after W2.4.
func triageCatalog() workload.Bundle {
	no := false
	yes := true
	return workload.Bundle{
		Name: "triage",
		ToolCatalog: workload.ToolCatalog{
			MCP: []workload.MCPServerRef{{Server: "gke"}},
			Tools: []workload.ToolPolicy{
				{Name: "get_k8s_resource", Mutating: &no},
				{Name: "list_k8s_events", Mutating: &no},
				{Name: "patch_resource", Mutating: &yes},
			},
		},
	}
}

func readOnlySpec(name string, tools ...string) specialists.Spec {
	return specialists.Spec{
		Name: name,
		Mode: specialists.ModeTask,
		Tools: specialists.ToolAllowlist{
			MCP: []specialists.MCPAllowlist{{Server: "gke", Tools: tools}},
		},
	}
}

func TestCheckCapabilitySplit(t *testing.T) {
	tests := []struct {
		name   string
		bundle workload.Bundle
		spec   specialists.Spec
		// wantErr, when non-empty, is a substring the refusal must
		// contain — always the specialist's name, and where there is one,
		// the tool that caused it. An operator reading a startup failure
		// at 3am should not have to grep the roster to find the line.
		wantErr []string
	}{
		{
			name:   "a diagnoser that enumerates read tools is fine",
			bundle: triageCatalog(),
			spec:   readOnlySpec("OOMKilled", "get_k8s_resource", "list_k8s_events"),
		},
		{
			name:    "a diagnoser holding a write tool is refused",
			bundle:  triageCatalog(),
			spec:    readOnlySpec("OOMKilled", "get_k8s_resource", "patch_resource"),
			wantErr: []string{"OOMKilled", "patch_resource"},
		},
		{
			// The failure mode the prose never caught: an unclassified
			// tool is mutating under default-deny, so a diagnoser that
			// quietly picks up a new server-side tool is refused before it
			// can call one.
			name:    "an unclassified tool counts as mutating",
			bundle:  triageCatalog(),
			spec:    readOnlySpec("OOMKilled", "get_k8s_resource", "delete_pod"),
			wantErr: []string{"OOMKilled", "delete_pod"},
		},
		{
			name:   "a whole-server grant is refused",
			bundle: triageCatalog(),
			spec: specialists.Spec{
				Name: "_fallback", Mode: specialists.ModeTask,
				Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{{Server: "gke"}}},
			},
			wantErr: []string{"_fallback", "gke", "no tools: list"},
		},
		{
			name:    "no allowlist at all is refused when the workload has a catalog",
			bundle:  triageCatalog(),
			spec:    specialists.Spec{Name: "_fallback", Mode: specialists.ModeTask},
			wantErr: []string{"_fallback", "whole tool catalog"},
		},
		{
			// The escape hatch for a specialist that reasons over its
			// inputs and touches nothing: `mcp: []`, which
			// pkg/specialists.filterToolsets enforces as deny-all.
			name:   "the deny-all allowlist passes",
			bundle: triageCatalog(),
			spec: specialists.Spec{
				Name: "_synthesis", Mode: specialists.ModeTask,
				Tools: specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{}},
			},
		},
		{
			name:   "a declared change executor may hold write tools",
			bundle: triageCatalog(),
			spec: specialists.Spec{
				Name: "change-executor", Mode: specialists.ModeTask,
				Capability: specialists.CapabilityChangeExecutor,
				Tools: specialists.ToolAllowlist{
					MCP: []specialists.MCPAllowlist{{Server: "gke", Tools: []string{"patch_resource"}}},
				},
			},
		},
		{
			// A SingleTurn classifier is built without toolsets, so it
			// cannot reach a tool of any class; making it enumerate an
			// allowlist it will never use would be ceremony.
			name:   "a SingleTurn classifier needs no allowlist",
			bundle: triageCatalog(),
			spec:   specialists.Spec{Name: "triage-classifier", Mode: specialists.ModeSingleTurn},
		},
		{
			// No catalog, nothing to inherit: the programmatic-embed case,
			// where toolsets arrive through BuildOptions instead.
			name:   "no catalog, no allowlist required",
			bundle: workload.Bundle{Name: "bare"},
			spec:   specialists.Spec{Name: "solo", Mode: specialists.ModeTask},
		},
		{
			name:   "a mutating built-in is refused too",
			bundle: workload.Bundle{Name: "bare"},
			spec: specialists.Spec{
				Name: "solo", Mode: specialists.ModeTask,
				Tools: specialists.ToolAllowlist{Builtin: []string{"apply_change"}},
			},
			wantErr: []string{"solo", "apply_change"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			specs := []specialists.Spec{tc.spec}
			err := CheckCapabilitySplit(tc.bundle, specs, MutationPredicate(tc.bundle, nil), nil)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("CheckCapabilitySplit = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckCapabilitySplit = nil, want a refusal mentioning %v", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// The write surface of a roster is the one thing an operator should be
// able to read off a startup log without opening the bundle, so the
// declaration is logged with the tools it covers.
func TestCheckCapabilitySplitLogsTheWriteSurface(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	b := triageCatalog()
	specs := []specialists.Spec{
		readOnlySpec("OOMKilled", "get_k8s_resource"),
		{
			Name: "change-executor", Mode: specialists.ModeTask,
			Capability: specialists.CapabilityChangeExecutor,
			Tools: specialists.ToolAllowlist{
				MCP: []specialists.MCPAllowlist{{Server: "gke", Tools: []string{"get_k8s_resource", "patch_resource"}}},
			},
		},
	}
	if err := CheckCapabilitySplit(b, specs, MutationPredicate(b, nil), logger); err != nil {
		t.Fatalf("CheckCapabilitySplit: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "change-executor") || !strings.Contains(out, "patch_resource") {
		t.Errorf("startup log does not name the write surface: %q", out)
	}
	if strings.Contains(out, "OOMKilled") {
		t.Errorf("read-only specialist logged as a write declaration: %q", out)
	}
}

// The check runs in BuildRoot, before any dispatch shape is assembled,
// so a roster that violates the split fails to start rather than
// failing on the turn that dispatches to the offending specialist. This
// covers the coordinator shape; the fan-out shape's fixtures in
// dispatch_test.go cover the other.
func TestBuildRootRefusesADiagnoserThatCanMutate(t *testing.T) {
	_, err := BuildRoot(context.Background(), RootConfig{
		Bundle:   triageCatalog(),
		Specs:    []specialists.Spec{readOnlySpec("OOMKilled", "patch_resource")},
		Model:    mastagent.NewEchoModel("echo"),
		Dispatch: DispatchCoordinator,
	})
	if err == nil {
		t.Fatal("BuildRoot accepted a read_only diagnoser holding patch_resource")
	}
	if !strings.Contains(err.Error(), "OOMKilled") || !strings.Contains(err.Error(), "patch_resource") {
		t.Fatalf("refusal does not name the specialist and the tool: %v", err)
	}
}
