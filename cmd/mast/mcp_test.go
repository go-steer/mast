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

	"github.com/go-steer/mast/pkg/mcp"
	"github.com/go-steer/mast/pkg/workload"
)

// bundleWithMCP builds a minimal bundle referencing the named MCP servers.
func bundleWithMCP(servers ...string) workload.Bundle {
	refs := make([]workload.MCPServerRef, len(servers))
	for i, s := range servers {
		refs[i] = workload.MCPServerRef{Server: s}
	}
	return workload.Bundle{ToolCatalog: workload.ToolCatalog{MCP: refs}}
}

// writeCatalogFile writes an mcp.json into dir and returns dir.
func writeCatalogFile(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, mcp.CatalogFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	return dir
}

// TestWireMCPToolsets_EchoSkips verifies the echo model wires nothing —
// even when the bundle references servers and no mcp.json exists, so the
// missing-catalog error must not fire.
func TestWireMCPToolsets_EchoSkips(t *testing.T) {
	ts, err := wireMCPToolsets(context.Background(), discardLogger(),
		bundleWithMCP("gke"), t.TempDir(), "echo")
	if err != nil {
		t.Fatalf("echo wiring should never error: %v", err)
	}
	if ts != nil {
		t.Errorf("echo wiring should produce no toolsets, got %d", len(ts))
	}
}

// TestWireMCPToolsets_NoRefsSkipsCatalog verifies a workload that declares
// no MCP servers does not require an mcp.json to exist.
func TestWireMCPToolsets_NoRefsSkipsCatalog(t *testing.T) {
	ts, err := wireMCPToolsets(context.Background(), discardLogger(),
		workload.Bundle{}, t.TempDir(), "scripted")
	if err != nil {
		t.Fatalf("no-ref wiring should not require a catalog: %v", err)
	}
	if ts != nil {
		t.Errorf("no-ref wiring should produce no toolsets, got %d", len(ts))
	}
}

// TestWireMCPToolsets_MissingCatalog verifies that referencing a server
// under a real model with no mcp.json present is a fatal error.
func TestWireMCPToolsets_MissingCatalog(t *testing.T) {
	_, err := wireMCPToolsets(context.Background(), discardLogger(),
		bundleWithMCP("gke"), t.TempDir(), "scripted")
	if err == nil {
		t.Fatal("expected error when mcp.json is absent but a server is referenced")
	}
}

// TestWireMCPToolsets_UnknownServer verifies the headline contract: a
// reference to a server absent from the catalog is fatal, not a
// silently-dropped tool.
func TestWireMCPToolsets_UnknownServer(t *testing.T) {
	dir := writeCatalogFile(t, t.TempDir(), `{
  "version": 1,
  "servers": {"gke": {"transport": "http", "url": "https://x"}}
}`)
	_, err := wireMCPToolsets(context.Background(), discardLogger(),
		bundleWithMCP("nonesuch"), dir, "scripted")
	if err == nil {
		t.Fatal("expected error for a server not defined in the catalog")
	}
	if !strings.Contains(err.Error(), "not defined in") {
		t.Errorf("error = %v, want 'not defined in'", err)
	}
}

// TestWireMCPToolsets_StdioWiresLazily verifies a defined stdio server is
// wired into a toolset without launching the process (a bogus command
// still wires cleanly; the launch is deferred to first tool use).
func TestWireMCPToolsets_StdioWiresLazily(t *testing.T) {
	dir := writeCatalogFile(t, t.TempDir(), `{
  "version": 1,
  "servers": {"blocker": {"transport": "stdio", "command": "/nonexistent/blocker"}}
}`)
	ts, err := wireMCPToolsets(context.Background(), discardLogger(),
		bundleWithMCP("blocker"), dir, "scripted")
	if err != nil {
		t.Fatalf("stdio wiring should be lazy and not error: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("wired toolsets = %d, want 1", len(ts))
	}
}
