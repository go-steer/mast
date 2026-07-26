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
	if b.EdgeTrigger.HTTP == nil || b.EdgeTrigger.HTTP.Path != "/inject" {
		t.Errorf("EdgeTrigger.HTTP unexpected: %+v", b.EdgeTrigger.HTTP)
	}
	if b.Filename != path {
		t.Errorf("Filename = %q, want %q", b.Filename, path)
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
