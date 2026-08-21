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
	"slices"
	"testing"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/workload"
)

// digestableWorkload writes a buildable one-specialist workload whose
// single MCP server is stdio, so wiring is lazy and needs no
// credentials and no live process.
func digestableWorkload(t *testing.T) string {
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
    "blocker": {
      "transport": "stdio",
      "command": "/nonexistent/mast-no-such-mcp-server"
    }
  }
}
`)
	write("workload.yaml", `name: digestable
description: Fixture for the MCP digest wiring tests.
mode: single_session
dispatch: coordinator

tool_catalog:
  mcp:
    - server: blocker
  tools:
    - name: read_status
      mutating: false

specialists:
  - analyst
`)
	write(filepath.Join("specialists", "analyst.tmpl"), `---
name: analyst
description: Reads the cluster and reports.
mode: Task
tools:
  mcp:
    - server: blocker
      tools: [read_status]
---

Report what you were given.
`)
	return dir
}

func catalogNames(t *testing.T, b rootBuild) []string {
	t.Helper()
	snap := b.catalog(discardLogger(), mutatingNames()).snapshot(context.Background())
	names := make([]string, 0, len(snap))
	for _, ti := range snap {
		if ti.Source == attach.ToolSourceBuiltin {
			names = append(names, ti.Name)
		}
	}
	return names
}

// /tools answers "what can this daemon do" (#205), and once the digest
// wrap is on, retrieve_raw is part of that answer — a tool specialists
// can genuinely call. #133's lesson was that a field nothing assigns
// still answers 200 with a plausible list, so this asserts on the
// published catalog rather than on the wiring call that fills it.
//
// The model name is load-bearing: MCP is not wired under echo, so under
// echo this test would pass with the wiring removed.
func TestBuildRootPublishesRetrieveRawWhenDigestingIsOn(t *testing.T) {
	built, err := buildRoot(context.Background(), discardLogger(),
		mastagent.NewToolActorModel("toolactor"), "", "toolactor",
		digestableWorkload(t), workload.DispatchCoordinator,
		hostSeams{digest: newDigestOptions(discardLogger(), true)})
	if err != nil {
		t.Fatalf("buildRoot: %v", err)
	}
	if got := catalogNames(t, built); !slices.Contains(got, "retrieve_raw") {
		t.Errorf("daemon builtins = %v, want retrieve_raw among them", got)
	}
}

// And under --mcp-digest=false it must not be there: a catalog naming a
// tool that does not exist is worse than one omitting a tool that does
// (the v0.3 tool_catalog finding).
func TestBuildRootOmitsRetrieveRawWhenDigestingIsOff(t *testing.T) {
	built, err := buildRoot(context.Background(), discardLogger(),
		mastagent.NewToolActorModel("toolactor"), "", "toolactor",
		digestableWorkload(t), workload.DispatchCoordinator,
		hostSeams{digest: newDigestOptions(discardLogger(), false)})
	if err != nil {
		t.Fatalf("buildRoot: %v", err)
	}
	if got := catalogNames(t, built); slices.Contains(got, "retrieve_raw") {
		t.Errorf("daemon builtins = %v under --mcp-digest=false, want no retrieve_raw", got)
	}
}
