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
//
// The config is validated here (the same per-server checks Catalog.Validate
// runs), so a config built directly in code — the library-embedded path,
// bypassing LoadCatalog — is held to the same rules as one parsed from
// mcp.json. This matters for the env-scoping fields: an unrecognized
// EnvMode must fail closed with an error rather than silently falling back
// to full daemon-environment inheritance in childEnv. The catalog-level
// command_allowlist is not enforced here because it is a Catalog policy,
// not a property of a single ServerConfig.
func NewToolset(ctx context.Context, name string, cfg ServerConfig) (tool.Toolset, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("mcp: server %q: %w", name, err)
	}
	var (
		ts  tool.Toolset
		err error
	)
	switch cfg.Transport {
	case TransportHTTP:
		ts, err = newHTTPToolset(ctx, name, cfg, nil)
	case TransportStdio:
		ts, err = newStdioToolset(name, cfg, nil)
	default:
		return nil, fmt.Errorf("mcp: server %q: unsupported transport %q (want %q or %q)",
			name, cfg.Transport, TransportHTTP, TransportStdio)
	}
	if err != nil {
		return nil, err
	}
	return named{name: name, Toolset: ts}, nil
}

// named gives a toolset the catalog key its server is declared under.
//
// ADK's mcptoolset reports a fixed Name() — "mcp_tool_set" — for every
// instance it builds, so without this wrapper a workload with three MCP
// servers hands the roster three identically-named toolsets and nothing
// downstream can tell them apart. That broke the per-specialist
// `tools.mcp: - server: <key>` allowlist outright: the whitelist path in
// pkg/specialists.filterToolsets matches allowlist entries to toolsets by
// Name(), so every lookup missed and the specialist was built with no MCP
// tools at all. Found 2026-08-14 in W2.4, when the first roster that
// enumerated its tools (rather than inheriting the catalog) got none.
//
// tool.FilterToolset delegates Name() to the toolset it wraps, so the
// narrowed toolsets filterToolsets returns keep the server key too.
type named struct {
	tool.Toolset
	name string
}

func (n named) Name() string { return n.name }

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
// against the daemon environment. The child's environment is assembled by
// childEnv per cfg.EnvMode.
func buildStdioCommand(cfg ServerConfig) *exec.Cmd {
	command, args := cfg.ResolvedCommand()
	// #nosec G204 -- launching a configured command is the whole point of
	// the stdio transport; mcp.json is operator-trusted control-plane
	// config (see catalog.go), optionally bounded by the catalog
	// command_allowlist. The permission gate write-protects the
	// `.agents/`-tree copy (and any path explicitly registered with it);
	// catalogs at other locations rely on filesystem permissions. The
	// resolved command + args are audit-logged at the wiring site.
	cmd := exec.Command(command, args...)
	cmd.Env = cfg.childEnv()
	return cmd
}

// childEnv builds the environment slice for a stdio child per EnvMode:
//
//   - inherit (default): returns nil when no env is configured so exec
//     inherits os.Environ() untouched; otherwise the daemon environment
//     with the configured env appended (see appendConfiguredEnv).
//   - clean: starts from an empty slice, copies only the EnvPassthrough
//     daemon variables that are actually set, then appends the configured
//     env. The result is non-nil even when empty, so exec gives the child
//     exactly this set (an empty environment) rather than the daemon's.
//
// In both modes the configured env is appended last, so a configured key
// overrides an inherited or passed-through one of the same name.
func (cfg ServerConfig) childEnv() []string {
	if cfg.EnvMode == EnvModeClean {
		env := make([]string, 0, len(cfg.EnvPassthrough)+len(cfg.Env))
		for _, k := range cfg.EnvPassthrough {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
		return appendConfiguredEnv(env, cfg.Env)
	}
	if len(cfg.Env) == 0 {
		return nil // inherit os.Environ() untouched
	}
	return appendConfiguredEnv(os.Environ(), cfg.Env)
}

// appendConfiguredEnv appends the configured env map to base in sorted key
// order (deterministic, and later entries win over earlier ones on
// collision) with ${VAR} expansion against the daemon environment. base is
// returned unchanged when extra is empty.
func appendConfiguredEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		base = append(base, k+"="+os.ExpandEnv(extra[k]))
	}
	return base
}
