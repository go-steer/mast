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

package mcp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/mcp"
)

func writeCatalog(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, mcp.CatalogFileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	return path
}

func TestLoadCatalog_HTTPAndStdio(t *testing.T) {
	path := writeCatalog(t, `{
  "version": 1,
  "servers": {
    "gke": {
      "transport": "http",
      "url": "https://container.googleapis.com/mcp",
      "auth": { "google_oauth": { "scopes": ["https://www.googleapis.com/auth/cloud-platform"] } }
    },
    "blocker": {
      "transport": "stdio",
      "command": "/usr/local/bin/blocker",
      "args": ["--flag", "${HOME}/x"],
      "env": { "TOKEN": "abc" }
    }
  }
}`)

	cat, err := mcp.LoadCatalog(path)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if cat.Version != 1 {
		t.Errorf("version = %d, want 1", cat.Version)
	}
	if len(cat.Servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(cat.Servers))
	}

	gke := cat.Servers["gke"]
	if gke.Transport != mcp.TransportHTTP {
		t.Errorf("gke transport = %q, want http", gke.Transport)
	}
	if gke.URL != "https://container.googleapis.com/mcp" {
		t.Errorf("gke url = %q", gke.URL)
	}
	if gke.Auth == nil || gke.Auth.GoogleOAuth == nil {
		t.Fatalf("gke auth.google_oauth missing")
	}
	if got := gke.Auth.GoogleOAuth.Scopes; len(got) != 1 || got[0] != "https://www.googleapis.com/auth/cloud-platform" {
		t.Errorf("gke scopes = %v", got)
	}

	b := cat.Servers["blocker"]
	if b.Transport != mcp.TransportStdio {
		t.Errorf("blocker transport = %q, want stdio", b.Transport)
	}
	if b.Command != "/usr/local/bin/blocker" {
		t.Errorf("blocker command = %q", b.Command)
	}
	if len(b.Args) != 2 || b.Args[0] != "--flag" || b.Args[1] != "${HOME}/x" {
		t.Errorf("blocker args = %v (expansion must happen at wire time, not parse time)", b.Args)
	}
	if b.Env["TOKEN"] != "abc" {
		t.Errorf("blocker env TOKEN = %q", b.Env["TOKEN"])
	}
}

func TestLoadCatalog_MissingFile(t *testing.T) {
	_, err := mcp.LoadCatalog(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing catalog file")
	}
}

func TestLoadCatalog_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "malformed json",
			body: `{"version": 1, "servers": {`,
			want: "parse catalog",
		},
		{
			name: "unsupported version",
			body: `{"version": 2, "servers": {}}`,
			want: "unsupported version",
		},
		{
			name: "missing version",
			body: `{"servers": {"x": {"transport": "http", "url": "https://x"}}}`,
			want: "unsupported version",
		},
		{
			name: "empty server name",
			body: `{"version": 1, "servers": {"": {"transport": "http", "url": "https://x"}}}`,
			want: "must not be empty",
		},
		{
			name: "missing transport",
			body: `{"version": 1, "servers": {"x": {"url": "https://x"}}}`,
			want: "missing transport",
		},
		{
			name: "unknown transport",
			body: `{"version": 1, "servers": {"x": {"transport": "grpc"}}}`,
			want: "unknown transport",
		},
		{
			name: "http without url",
			body: `{"version": 1, "servers": {"x": {"transport": "http"}}}`,
			want: "http transport requires a url",
		},
		{
			name: "stdio without command",
			body: `{"version": 1, "servers": {"x": {"transport": "stdio"}}}`,
			want: "stdio transport requires a command",
		},
		{
			name: "unknown env_mode",
			body: `{"version": 1, "servers": {"x": {"transport": "stdio", "command": "/bin/true", "env_mode": "sandbox"}}}`,
			want: "unknown env_mode",
		},
		{
			name: "env_passthrough without clean mode",
			body: `{"version": 1, "servers": {"x": {"transport": "stdio", "command": "/bin/true", "env_passthrough": ["PATH"]}}}`,
			want: "env_passthrough requires env_mode",
		},
		{
			name: "command not in allowlist",
			body: `{"version": 1, "command_allowlist": ["/usr/bin/allowed"], "servers": {"x": {"transport": "stdio", "command": "/bin/evil"}}}`,
			want: "not in the catalog command_allowlist",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mcp.LoadCatalog(writeCatalog(t, tc.body))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

// TestLoadCatalog_CommandAllowlist verifies the catalog-level command
// allowlist (#89 item 3): an empty list imposes no restriction, a
// disallowed stdio command fails validation, an allowed one passes, and
// both the entry and the resolved command are ${VAR}-expanded before the
// membership check (so an allowlist written with a variable matches a
// command written with the same variable).
func TestLoadCatalog_CommandAllowlist(t *testing.T) {
	t.Run("empty allowlist imposes no restriction", func(t *testing.T) {
		_, err := mcp.LoadCatalog(writeCatalog(t, `{
  "version": 1,
  "servers": {"x": {"transport": "stdio", "command": "/anything/goes"}}
}`))
		if err != nil {
			t.Fatalf("empty allowlist should allow any command: %v", err)
		}
	})

	t.Run("allowed command passes", func(t *testing.T) {
		_, err := mcp.LoadCatalog(writeCatalog(t, `{
  "version": 1,
  "command_allowlist": ["/usr/bin/allowed", "/bin/true"],
  "servers": {"x": {"transport": "stdio", "command": "/bin/true"}}
}`))
		if err != nil {
			t.Fatalf("allowed command should pass: %v", err)
		}
	})

	t.Run("http servers are not allowlist-checked", func(t *testing.T) {
		// The allowlist bounds only launchable commands; an HTTP server
		// has none, so it must not be rejected by a command allowlist.
		_, err := mcp.LoadCatalog(writeCatalog(t, `{
  "version": 1,
  "command_allowlist": ["/bin/true"],
  "servers": {"x": {"transport": "http", "url": "https://x"}}
}`))
		if err != nil {
			t.Fatalf("http server should not be command-allowlist checked: %v", err)
		}
	})

	t.Run("expansion applies to both sides", func(t *testing.T) {
		t.Setenv("MAST_TEST_BINDIR", "/opt/mcp/bin")
		_, err := mcp.LoadCatalog(writeCatalog(t, `{
  "version": 1,
  "command_allowlist": ["${MAST_TEST_BINDIR}/server"],
  "servers": {"x": {"transport": "stdio", "command": "${MAST_TEST_BINDIR}/server"}}
}`))
		if err != nil {
			t.Fatalf("expanded command should match expanded allowlist entry: %v", err)
		}
	})
}

// TestLoadCatalog_CleanEnvMode confirms the parser accepts and preserves
// the stdio env-scoping fields (#89 item 2).
func TestLoadCatalog_CleanEnvMode(t *testing.T) {
	cat, err := mcp.LoadCatalog(writeCatalog(t, `{
  "version": 1,
  "servers": {"x": {"transport": "stdio", "command": "/bin/true", "env_mode": "clean", "env_passthrough": ["PATH", "HOME"]}}
}`))
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	x := cat.Servers["x"]
	if x.EnvMode != mcp.EnvModeClean {
		t.Errorf("env_mode = %q, want %q", x.EnvMode, mcp.EnvModeClean)
	}
	if len(x.EnvPassthrough) != 2 || x.EnvPassthrough[0] != "PATH" || x.EnvPassthrough[1] != "HOME" {
		t.Errorf("env_passthrough = %v", x.EnvPassthrough)
	}
}

// TestLoadCatalog_ToleratesUnknownFields keeps the parser forward
// compatible with richer mcp.json files (allowlist, credentials, …) the
// design describes but this build does not yet consume.
func TestLoadCatalog_ToleratesUnknownFields(t *testing.T) {
	path := writeCatalog(t, `{
  "version": 1,
  "servers": {
    "x": {
      "transport": "stdio",
      "command": "/bin/true",
      "allowlist": {"tools": ["a"]},
      "credentials": {"kind": "env"}
    }
  }
}`)
	cat, err := mcp.LoadCatalog(path)
	if err != nil {
		t.Fatalf("LoadCatalog with unknown fields: %v", err)
	}
	if cat.Servers["x"].Command != "/bin/true" {
		t.Errorf("command = %q", cat.Servers["x"].Command)
	}
}
