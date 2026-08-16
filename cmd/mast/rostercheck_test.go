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

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/workload"
)

// unbuildableBounded writes a workload that is wrong in two independent
// ways at once: its roster can never be `bounded` (two specialists), and
// its one MCP server cannot be wired (the catalog does not define it).
//
// The wiring fault is an undefined server rather than an unreachable
// one because only the first is eager: mast launches a stdio server
// lazily, on first tool use, so a command that does not exist wires
// perfectly well and would make this fixture prove nothing.
//
// Which error mast reports is the whole subject of the test below.
func unbuildableBounded(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "specialists"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("mcp.json", `{
  "version": 1,
  "servers": {
    "some-other-server": {
      "transport": "stdio",
      "command": "/nonexistent/mast-no-such-mcp-server"
    }
  }
}
`)
	write("workload.yaml", `name: unbuildable
description: Fixture for TestBuildRootRefusesTheRosterBeforeWiringMCP.
mode: single_session
dispatch: bounded

tool_catalog:
  mcp:
    - server: unreachable
  tools:
    - name: read_status
      mutating: false

specialists:
  - first
  - second
`)
	for _, name := range []string{"first", "second"} {
		write(filepath.Join("specialists", name+".tmpl"), `---
name: `+name+`
description: One of two specialists, which is one too many for bounded.
mode: SingleTurn
tools:
  mcp: []
---

Report what you were given.
`)
	}
	return dir
}

// TestBuildRootRefusesTheRosterBeforeWiringMCP pins an ordering, not a
// message: the roster check is a pure function of the bundle and the
// specs, and it has to run before mast spawns an MCP server process or
// fetches a token for a hosted one.
//
// The bug this is the regression for was found in CI rather than
// locally, which is the tell. `wireMCPToolsets` is skipped under
// --model=echo, so every offline test walked past it; the acceptance
// leg that points --dispatch=bounded at the fourteen-specialist
// gke-triage roster runs under --model=toolactor, which does wire it,
// and on a runner with no Google credentials the operator's reason for
// "mast will not start" came back as an OAuth failure against the
// hosted GKE MCP server. The roster was never going to become
// buildable if that token had been fetched.
//
// A model name other than echo is therefore load-bearing here: under
// echo this test would pass without the fix.
func TestBuildRootRefusesTheRosterBeforeWiringMCP(t *testing.T) {
	_, err := buildRoot(context.Background(), discardLogger(),
		mastagent.NewToolActorModel("toolactor"), "", "toolactor",
		unbuildableBounded(t), workload.DispatchBounded, nil)
	if err == nil {
		t.Fatal("buildRoot accepted a two-specialist bounded roster; want a refusal")
	}
	if strings.Contains(err.Error(), "MCP server") {
		t.Fatalf("mast reached MCP before checking the roster, and the refusal an operator sees is the wiring failure: %v", err)
	}
	for _, want := range []string{"2 specialists", "first, second", "takes exactly one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}
