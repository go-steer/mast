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

package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// roCtx is a minimal agent.ReadonlyContext double. mcptoolset.Tools only
// uses the embedded context.Context (to ListTools over the transport), so
// the accessor methods can return zero values.
type roCtx struct {
	context.Context
}

func (roCtx) UserContent() *genai.Content          { return nil }
func (roCtx) InvocationID() string                 { return "" }
func (roCtx) AgentName() string                    { return "" }
func (roCtx) ReadonlyState() session.ReadonlyState { return nil }
func (roCtx) UserID() string                       { return "" }
func (roCtx) AppName() string                      { return "" }
func (roCtx) SessionID() string                    { return "" }
func (roCtx) Branch() string                       { return "" }

func TestBuildStdioCommand_ExpandsAndSortsEnv(t *testing.T) {
	t.Setenv("MAST_TEST_HOME", "/home/tester")
	t.Setenv("MAST_TEST_TOKEN", "sekret")

	cfg := ServerConfig{
		Transport: TransportStdio,
		Command:   "${MAST_TEST_HOME}/bin/server",
		Args:      []string{"--data", "${MAST_TEST_HOME}/data", "--literal"},
		// Keys deliberately supplied out of order (and a third middle key)
		// so a non-sorting implementation cannot pass by luck.
		Env: map[string]string{
			"ZED":   "z",
			"ALPHA": "${MAST_TEST_TOKEN}",
			"MIKE":  "m",
		},
	}

	cmd := buildStdioCommand(cfg)

	if cmd.Path != "/home/tester/bin/server" && !strings.HasSuffix(cmd.Path, "/home/tester/bin/server") {
		// exec.Command may resolve Path; Args[0] carries the requested command verbatim.
		t.Errorf("cmd.Path = %q, want expanded command", cmd.Path)
	}
	if got := cmd.Args[0]; got != "/home/tester/bin/server" {
		t.Errorf("cmd.Args[0] = %q, want expanded command", got)
	}
	wantArgs := []string{"/home/tester/bin/server", "--data", "/home/tester/data", "--literal"}
	if !slices.Equal(cmd.Args, wantArgs) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, wantArgs)
	}

	// Configured env is expanded and appended in sorted key order after
	// the inherited environment, so it overrides and is deterministic.
	last := cmd.Env[len(cmd.Env)-3:]
	wantTail := []string{"ALPHA=sekret", "MIKE=m", "ZED=z"}
	if !slices.Equal(last, wantTail) {
		t.Errorf("env tail = %v, want %v (sorted, expanded, appended last)", last, wantTail)
	}

	// The child inherits the daemon environment: a var neither expanded
	// nor overridden must still be present.
	t.Setenv("MAST_TEST_INHERITED", "yes")
	cmd2 := buildStdioCommand(cfg)
	if !slices.Contains(cmd2.Env, "MAST_TEST_INHERITED=yes") {
		t.Error("child env does not inherit the daemon environment")
	}
}

func TestBuildStdioCommand_NoEnvLeavesInherited(t *testing.T) {
	cfg := ServerConfig{Transport: TransportStdio, Command: "/bin/true"}
	cmd := buildStdioCommand(cfg)
	// With no configured env, cmd.Env is left nil so exec uses os.Environ().
	if cmd.Env != nil {
		t.Errorf("cmd.Env = %v, want nil (inherit os.Environ)", cmd.Env)
	}
}

// TestBuildStdioCommand_ConfiguredEnvOverridesInherited verifies that a
// configured env key which collides with an inherited variable wins:
// because configured entries are appended last, exec resolves the final
// occurrence, so the child sees the catalog value, not the daemon's.
func TestBuildStdioCommand_ConfiguredEnvOverridesInherited(t *testing.T) {
	t.Setenv("MAST_TEST_COLLIDE", "inherited-value")
	cfg := ServerConfig{
		Transport: TransportStdio,
		Command:   "/bin/true",
		Env:       map[string]string{"MAST_TEST_COLLIDE": "configured-value"},
	}
	cmd := buildStdioCommand(cfg)

	// The inherited copy appears first (from os.Environ); the configured
	// copy must be appended after it and be the last occurrence.
	last := -1
	for i, e := range cmd.Env {
		if strings.HasPrefix(e, "MAST_TEST_COLLIDE=") {
			last = i
		}
	}
	if last == -1 {
		t.Fatal("MAST_TEST_COLLIDE not present in child env")
	}
	if cmd.Env[last] != "MAST_TEST_COLLIDE=configured-value" {
		t.Errorf("last MAST_TEST_COLLIDE = %q, want the configured value to win", cmd.Env[last])
	}
}

// TestNewToolset_HTTPNoAuthConstructs verifies an HTTP server without an
// auth block constructs offline (unauthenticated endpoint): the session,
// and thus any network call, is deferred to first use.
func TestNewToolset_HTTPNoAuthConstructs(t *testing.T) {
	ts, err := NewToolset(context.Background(), "x", ServerConfig{
		Transport: TransportHTTP,
		URL:       "https://example.invalid/mcp",
	})
	if err != nil {
		t.Fatalf("NewToolset(http, no auth) = %v, want lazy construction", err)
	}
	if ts == nil {
		t.Fatal("NewToolset returned nil toolset")
	}
}

func TestNewToolset_UnsupportedTransport(t *testing.T) {
	_, err := NewToolset(context.Background(), "x", ServerConfig{Transport: "grpc"})
	if err == nil || !strings.Contains(err.Error(), "unsupported transport") {
		t.Fatalf("err = %v, want unsupported transport", err)
	}
}

func TestNewToolset_StdioRequiresCommand(t *testing.T) {
	_, err := NewToolset(context.Background(), "x", ServerConfig{Transport: TransportStdio})
	if err == nil || !strings.Contains(err.Error(), "requires a command") {
		t.Fatalf("err = %v, want requires a command", err)
	}
}

func TestNewToolset_HTTPRequiresURL(t *testing.T) {
	_, err := NewToolset(context.Background(), "x", ServerConfig{Transport: TransportHTTP})
	if err == nil || !strings.Contains(err.Error(), "requires a url") {
		t.Fatalf("err = %v, want requires a url", err)
	}
}

// TestNewToolset_StdioLazy verifies construction does not launch the child
// process: a bogus command still constructs cleanly (the session, and thus
// the exec, is deferred to first use).
func TestNewToolset_StdioLazy(t *testing.T) {
	ts, err := NewToolset(context.Background(), "x", ServerConfig{
		Transport: TransportStdio,
		Command:   "/nonexistent/definitely/not/here",
	})
	if err != nil {
		t.Fatalf("NewToolset(stdio) = %v, want lazy construction", err)
	}
	if ts == nil {
		t.Fatal("NewToolset returned nil toolset")
	}
}

// TestStdioRoundTrip builds the testdata mcpserver helper and lists its
// tools over the real stdio transport, exercising the full local-process
// path (launch, JSON handshake, ListTools) and env passthrough.
func TestStdioRoundTrip(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; skipping stdio round-trip")
	}

	bin := filepath.Join(t.TempDir(), "mcpserver")
	build := exec.Command(goBin, "build", "-o", bin, "./testdata/mcpserver")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build testdata mcpserver: %v\n%s", err, out)
	}

	cases := []struct {
		name     string
		env      map[string]string
		wantTool string
	}{
		{name: "default tool name", wantTool: "ping"},
		{name: "env override", env: map[string]string{"MCP_TEST_TOOL_NAME": "blocker-do"}, wantTool: "blocker-do"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := NewToolset(context.Background(), "blocker", ServerConfig{
				Transport: TransportStdio,
				Command:   bin,
				Env:       tc.env,
			})
			if err != nil {
				t.Fatalf("NewToolset: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tools, err := ts.Tools(roCtx{Context: ctx})
			if err != nil {
				t.Fatalf("Tools: %v", err)
			}
			var names []string
			for _, tl := range tools {
				names = append(names, tl.Name())
			}
			if !slices.Contains(names, tc.wantTool) {
				t.Errorf("tools = %v, want to contain %q", names, tc.wantTool)
			}
		})
	}
}
