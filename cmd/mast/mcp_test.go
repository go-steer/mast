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

	"github.com/go-steer/mast/pkg/digest"
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
	ts, _, err := wireMCPToolsets(context.Background(), discardLogger(),
		bundleWithMCP("gke"), t.TempDir(), "echo", nil)
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
	ts, _, err := wireMCPToolsets(context.Background(), discardLogger(),
		workload.Bundle{}, t.TempDir(), "scripted", nil)
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
	_, _, err := wireMCPToolsets(context.Background(), discardLogger(),
		bundleWithMCP("gke"), t.TempDir(), "scripted", nil)
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
	_, _, err := wireMCPToolsets(context.Background(), discardLogger(),
		bundleWithMCP("nonesuch"), dir, "scripted", nil)
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
	ts, _, err := wireMCPToolsets(context.Background(), discardLogger(),
		bundleWithMCP("blocker"), dir, "scripted", nil)
	if err != nil {
		t.Fatalf("stdio wiring should be lazy and not error: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("wired toolsets = %d, want 1", len(ts))
	}
}

// twoServerCatalog defines one ordinary server and one that opted out of
// digesting with `no_digest: true`.
const twoServerCatalog = `{
  "version": 1,
  "servers": {
    "gke": {"transport": "stdio", "command": "/nonexistent/gke"},
    "logs": {"transport": "stdio", "command": "/nonexistent/logs", "no_digest": true}
  }
}`

// The escape hatch is the other half of the wrap: a digested response
// the model cannot un-digest is a lossy tool call with no appeal. So
// retrieve_raw is registered exactly when something was wrapped and
// there is a store to answer it from — the four cases below are the
// whole decision (#221).
func TestWireMCPToolsets_RetrieveRawRidesWithTheWrap(t *testing.T) {
	store, err := digest.NewFilesystemStore(filepath.Join(t.TempDir(), "raw"))
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}
	tests := []struct {
		name    string
		servers []string
		opts    *mcp.DigestOptions
		want    bool
	}{
		{
			name:    "a wrapped server and a store",
			servers: []string{"gke"},
			opts:    &mcp.DigestOptions{Store: store},
			want:    true,
		},
		{
			name:    "digesting off (--mcp-digest=false)",
			servers: []string{"gke"},
			opts:    nil,
		},
		{
			name:    "no store, so nothing to retrieve",
			servers: []string{"gke"},
			opts:    &mcp.DigestOptions{},
		},
		{
			name:    "every referenced server opted out",
			servers: []string{"logs"},
			opts:    &mcp.DigestOptions{Store: store},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeCatalogFile(t, t.TempDir(), twoServerCatalog)
			ts, extra, err := wireMCPToolsets(context.Background(), discardLogger(),
				bundleWithMCP(tc.servers...), dir, "scripted", tc.opts)
			if err != nil {
				t.Fatalf("wireMCPToolsets: %v", err)
			}
			// Wrapped or not, the toolsets themselves always come
			// through: specialists.filterToolsets matches them to a
			// `tools.mcp: - server:` entry by name.
			if len(ts) != len(tc.servers) {
				t.Fatalf("wired toolsets = %d, want %d", len(ts), len(tc.servers))
			}
			for i, want := range tc.servers {
				if ts[i].Name() != want {
					t.Errorf("toolset %d is named %q, want the catalog key %q", i, ts[i].Name(), want)
				}
			}
			if got := len(extra) == 1 && extra[0].Name() == mcp.RetrieveRawToolName; got != tc.want {
				t.Errorf("retrieve_raw registered = %v, want %v (extras: %d)", got, tc.want, len(extra))
			}
		})
	}
}

// A server that opts out must not drag the rest of the catalog with it:
// `no_digest: true` is a per-server escape hatch, not a daemon switch.
func TestWireMCPToolsets_OptOutIsPerServer(t *testing.T) {
	store, err := digest.NewFilesystemStore(filepath.Join(t.TempDir(), "raw"))
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}
	dir := writeCatalogFile(t, t.TempDir(), twoServerCatalog)
	_, extra, err := wireMCPToolsets(context.Background(), discardLogger(),
		bundleWithMCP("gke", "logs"), dir, "scripted", &mcp.DigestOptions{Store: store})
	if err != nil {
		t.Fatalf("wireMCPToolsets: %v", err)
	}
	if len(extra) != 1 || extra[0].Name() != mcp.RetrieveRawToolName {
		t.Errorf("extras = %d, want retrieve_raw — one opted-out server disabled the whole wrap", len(extra))
	}
}

// newDigestOptions owns two things worth pinning: the kill switch, and
// where the raw payloads land. They are scratch state that dies with the
// run, so they go under os.TempDir() and never in $HOME (house rule #5).
func TestNewDigestOptions(t *testing.T) {
	if got := newDigestOptions(discardLogger(), false); got != nil {
		t.Errorf("--mcp-digest=false should turn the wrap off entirely, got %#v", got)
	}
	opts := newDigestOptions(discardLogger(), true)
	if opts == nil {
		t.Fatal("--mcp-digest=true should produce options")
	}
	if opts.Store == nil {
		t.Error("no store: retrieve_raw would go unregistered on every run")
	}
	fs, ok := opts.Store.(*digest.FilesystemStore)
	if !ok {
		t.Fatalf("store is %T, want a *digest.FilesystemStore", opts.Store)
	}
	if !strings.HasPrefix(fs.Dir, os.TempDir()) {
		t.Errorf("raw payloads land in %q, want somewhere under %q", fs.Dir, os.TempDir())
	}
}
