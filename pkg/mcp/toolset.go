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
	"fmt"
	"os"
	"os/exec"
	"sort"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"
)

// NewToolset builds a tool.Toolset for a single catalog server, dispatched
// by transport kind. The MCP session is established lazily on first use,
// so this does not launch a stdio command or open an HTTP connection —
// except that HTTP servers with Google OAuth pre-fetch a token at
// construction to fail fast on missing credentials.
func NewToolset(ctx context.Context, name string, cfg ServerConfig) (tool.Toolset, error) {
	switch cfg.Transport {
	case TransportHTTP:
		return newHTTPToolset(ctx, name, cfg, nil)
	case TransportStdio:
		return newStdioToolset(name, cfg, nil)
	default:
		return nil, fmt.Errorf("mcp: server %q: unsupported transport %q (want %q or %q)",
			name, cfg.Transport, TransportHTTP, TransportStdio)
	}
}

// newHTTPToolset wires a streamable-HTTP MCP server. When the config
// declares Google OAuth, requests carry an ADC bearer token (fail-fast if
// credentials are unavailable); otherwise the endpoint is called
// unauthenticated. filter, when non-nil, narrows the exposed tools.
func newHTTPToolset(ctx context.Context, name string, cfg ServerConfig, filter tool.Predicate) (tool.Toolset, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp: server %q: http transport requires a url", name)
	}

	transport := &mcpsdk.StreamableClientTransport{Endpoint: cfg.URL}
	if cfg.Auth != nil && cfg.Auth.GoogleOAuth != nil {
		scopes := cfg.Auth.GoogleOAuth.Scopes
		if len(scopes) == 0 {
			scopes = []string{DefaultGKEScope}
		}
		httpClient, err := newGoogleAuthClient(ctx, name, scopes)
		if err != nil {
			return nil, err
		}
		transport.HTTPClient = httpClient
	}

	ts, err := mcptoolset.New(mcptoolset.Config{
		Transport:  transport,
		ToolFilter: filter,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: %q: construct toolset: %w", name, err)
	}
	return ts, nil
}

// newStdioToolset wires a local-process MCP server launched over stdio.
// mcptoolset rejects an Auth credential provider on a non-HTTP transport,
// so stdio servers authenticate (if at all) through their environment —
// see buildStdioCommand.
func newStdioToolset(name string, cfg ServerConfig, filter tool.Predicate) (tool.Toolset, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("mcp: server %q: stdio transport requires a command", name)
	}
	transport := &mcpsdk.CommandTransport{Command: buildStdioCommand(cfg)}
	ts, err := mcptoolset.New(mcptoolset.Config{
		Transport:  transport,
		ToolFilter: filter,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: %q: construct toolset: %w", name, err)
	}
	return ts, nil
}

// ResolvedCommand returns the executable and arguments a stdio server
// will launch, with ${VAR} references expanded against the daemon
// environment. Exposed so the wiring site can audit-log exactly what will
// run (command *and* args — the security-relevant payload often lives in
// the args) before the lazy launch.
func (cfg ServerConfig) ResolvedCommand() (string, []string) {
	args := make([]string, len(cfg.Args))
	for i, a := range cfg.Args {
		args[i] = os.ExpandEnv(a)
	}
	return os.ExpandEnv(cfg.Command), args
}

// buildStdioCommand constructs the exec.Cmd for a stdio MCP server.
// ${VAR} references in the command, args, and env values are expanded
// against the daemon environment. The child inherits the daemon
// environment; configured env entries are appended in sorted order so
// they override the inherited values (and so the result is deterministic).
func buildStdioCommand(cfg ServerConfig) *exec.Cmd {
	command, args := cfg.ResolvedCommand()
	// #nosec G204 -- launching a configured command is the whole point of
	// the stdio transport; mcp.json is operator-trusted control-plane
	// config (see catalog.go). The permission gate write-protects the
	// `.agents/`-tree copy; catalogs at other locations rely on filesystem
	// permissions. The resolved command + args are audit-logged at the
	// wiring site.
	cmd := exec.Command(command, args...)
	if len(cfg.Env) > 0 {
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		env := os.Environ()
		for _, k := range keys {
			env = append(env, k+"="+os.ExpandEnv(cfg.Env[k]))
		}
		cmd.Env = env
	}
	return cmd
}
