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
	"os"
	"path/filepath"
	"testing"

	"github.com/go-steer/mast/pkg/workload"
)

const validBundle = `name: gke-triage
description: Autonomous triage of GKE cluster incidents.
mode: single_session
tool_catalog:
  mcp:
    - server: gke
specialists:
  - triage-classifier
  - ImagePullBackOff
  - _fallback
budget:
  max_wallclock_seconds: 300
  max_turns: 20
edge_trigger:
  http:
    path: /inject
    auth: bearer
`

func writeBundle(t *testing.T, name, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := writeBundle(t, "gke-triage.yaml", validBundle)
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := b.Name, "gke-triage"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got, want := b.Mode, workload.ModeSingleSession; got != want {
		t.Errorf("Mode = %q, want %q", got, want)
	}
	if got, want := len(b.Specialists), 3; got != want {
		t.Fatalf("Specialists count = %d, want %d", got, want)
	}
	if got, want := b.Specialists[1], "ImagePullBackOff"; got != want {
		t.Errorf("Specialists[1] = %q, want %q", got, want)
	}
	if got, want := len(b.ToolCatalog.MCP), 1; got != want || b.ToolCatalog.MCP[0].Server != "gke" {
		t.Errorf("ToolCatalog.MCP unexpected: %+v", b.ToolCatalog.MCP)
	}
	if got, want := b.Budget.MaxWallclockSeconds, 300; got != want {
		t.Errorf("Budget.MaxWallclockSeconds = %d, want %d", got, want)
	}
	if got, want := b.Budget.MaxTurns, 20; got != want {
		t.Errorf("Budget.MaxTurns = %d, want %d", got, want)
	}
	if b.EdgeTrigger.HTTP == nil || b.EdgeTrigger.HTTP.Path != "/inject" {
		t.Errorf("EdgeTrigger.HTTP unexpected: %+v", b.EdgeTrigger.HTTP)
	}
	if b.Filename != path {
		t.Errorf("Filename = %q, want %q", b.Filename, path)
	}
}

func TestLoad_PlannerEnabled(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nplanner:\n  enabled: true\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !b.Planner.Enabled {
		t.Error("Planner.Enabled = false, want true")
	}
	// And the default stays off.
	path = writeBundle(t, "c.yaml", "name: x\nspecialists: [a]\n")
	b, err = workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if b.Planner.Enabled {
		t.Error("Planner.Enabled = true by default, want false")
	}
}

func TestLoad_DefaultsMode(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := b.Mode, workload.ModeSingleSession; got != want {
		t.Errorf("Mode = %q, want %q (default)", got, want)
	}
}

// A bundle names its own dispatch shape because the shape is a property
// of the roster, not of the invocation. An unset `dispatch:` stays
// empty rather than defaulting here: the default lives at the call site
// (cmd/mast resolves an unset flag AND an unset bundle to coordinator),
// so a loader default would take that choice away from a library
// caller.
func TestLoad_Dispatch(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\ndispatch: fanout\nfanout:\n  max_concurrency: 2\nspecialists: [a, _synthesis]\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if b.Dispatch != workload.DispatchFanout {
		t.Errorf("Dispatch = %q, want %q", b.Dispatch, workload.DispatchFanout)
	}
	if b.Fanout.MaxConcurrency != 2 {
		t.Errorf("Fanout.MaxConcurrency = %d, want 2", b.Fanout.MaxConcurrency)
	}

	bounded := writeBundle(t, "d.yaml", "name: x\ndispatch: bounded\nspecialists: [a]\n")
	bb, err := workload.Load(bounded)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if bb.Dispatch != workload.DispatchBounded {
		t.Errorf("Dispatch = %q, want %q", bb.Dispatch, workload.DispatchBounded)
	}

	silent := writeBundle(t, "c.yaml", "name: x\nspecialists: [a]\n")
	sb, err := workload.Load(silent)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sb.Dispatch != "" {
		t.Errorf("Dispatch = %q on a bundle that declares none, want empty", sb.Dispatch)
	}
}

func TestLoad_Errors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"missing name", "specialists: [a]\n", "name is required"},
		{"empty roster", "name: x\nspecialists: []\n", "specialists roster is empty"},
		{"duplicate", "name: x\nspecialists: [a, b, a]\n", "duplicate"},
		{"bad mode", "name: x\nmode: nonsense\nspecialists: [a]\n", "unknown mode"},
		{"bad dispatch", "name: x\ndispatch: sideways\nspecialists: [a]\n", "unknown dispatch"},
		// The plausible typo, not just the absurd one: `bound` has to be
		// refused by name, because a dispatch the loader shrugs at is a
		// bundle that runs as a coordinator while its author believes it
		// costs one call.
		{"a near-miss dispatch", "name: x\ndispatch: bound\nspecialists: [a]\n", "unknown dispatch"},
		// Same reasoning one field over, and it bites harder: a posture
		// the loader shrugs at is a workload that silently runs on the
		// host default while its author believes the halt is armed.
		{"bad safety.watchdog", "name: x\nspecialists: [a]\nsafety:\n  watchdog: halt\n", "unknown safety.watchdog"},
		{"a near-miss posture", "name: x\nspecialists: [a]\nsafety:\n  watchdog: enfoce\n", "unknown safety.watchdog"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeBundle(t, "b.yaml", tc.body)
			_, err := workload.Load(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want to contain %q", err, tc.wantErr)
			}
		})
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestLoad_ToolPolicies(t *testing.T) {
	path := writeBundle(t, "tools.yaml", `name: x
specialists: [a]
tool_catalog:
  mcp:
    - server: gke
  tools:
    - name: list_clusters
      mutating: false
    - name: rollout_undo
      mutating: true
    - name: no_override
`)
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tools := b.ToolCatalog.Tools
	if len(tools) != 3 {
		t.Fatalf("parsed %d tool policies, want 3", len(tools))
	}
	if tools[0].Name != "list_clusters" || tools[0].Mutating == nil || *tools[0].Mutating {
		t.Errorf("list_clusters policy = %+v, want mutating:false", tools[0])
	}
	if tools[1].Mutating == nil || !*tools[1].Mutating {
		t.Errorf("rollout_undo policy = %+v, want mutating:true", tools[1])
	}
	if tools[2].Mutating != nil {
		t.Errorf("no_override policy = %+v, want nil Mutating (omitted != false)", tools[2])
	}
}

func TestLoad_ToolPolicyErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantErr string
	}{
		{"unnamed", "name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - mutating: false\n", "without a name"},
		{"duplicate", "name: x\nspecialists: [a]\ntool_catalog:\n  tools:\n    - name: t\n    - name: t\n", "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeBundle(t, "b.yaml", tc.body)
			if _, err := workload.Load(path); err == nil || !contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want to contain %q", err, tc.wantErr)
			}
		})
	}
}
