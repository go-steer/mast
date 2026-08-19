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
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sort"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"github.com/go-steer/mast/internal/version"
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

// serverInitiatedInput are the requests an MCP server can send *to* the
// client in the middle of a call the client already made. mast refuses all
// three; see newMCPClient for why, and refuseServerInitiatedInput for the
// mechanism.
//
// SEP-2577 deprecated roots and sampling as of protocol 2026-07-28, which
// changes nothing here: the deprecation window is at least twelve months, the
// SDK still speaks both, and roots is the one a server can complete against a
// default client today. Drop a line only once the SDK stops answering it.
var serverInitiatedInput = []string{
	"elicitation/create",     // ask the operator a question
	"sampling/createMessage", // borrow the client's model
	"roots/list",             // enumerate the client's roots
}

// refuseServerInitiatedInput rejects every server-to-client input request
// at the receiving edge, before the SDK looks for a handler.
//
// The refusal has to sit here rather than rely on there being no handler,
// because two of the three do not fail on their own. roots/list is answered
// by the SDK itself from the client's own root set — no handler, no
// capability opt-in, no error — so a server can complete a round trip with
// mast today. And the multi-round-trip machinery treats a fulfilled request
// as licence to retry the original call, which re-dispatches a tool the
// approval gate cleared exactly once.
//
// Refusing by method also means a future elicitation handler, added for a
// good reason, cannot quietly re-open the path: whoever adds it has to
// delete a line here, which is a decision someone reviews.
func refuseServerInitiatedInput() mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if slices.Contains(serverInitiatedInput, method) {
				return nil, fmt.Errorf("mcp: refusing server-initiated %s: mast's approval gate covers tool dispatch, not an input request inside a call already in flight", method)
			}
			return next(ctx, method, req)
		}
	}
}

// newMCPClient builds the MCP client both transports connect through.
//
// It differs from the SDK's default client in three ways, all of them the
// same decision: a server does not get to ask mast for input mid-call.
//
// SEP-2322 multi-round-trip is disabled. go-sdk installs that middleware on
// every client it builds (mcp/mrtr.go:20-23) and it "automatically fulfills
// input requests from the server by invoking the appropriate client
// handlers and retrying the original call" — up to ten times. None of it
// traverses mast's approval gate, which sits on tool dispatch, not on a
// round trip inside a call in flight. With it off the SDK returns the
// input-required result to the caller (CallToolResult.NeedsInput) and the
// caller owns the retry loop, which is where a gate would go if mast ever
// decides to support elicitation.
//
// That flag alone is not enough, which is the part not obvious from the
// SDK's documentation. The client-side middleware only runs on protocol
// >= 2026-07-28, and StreamableServerTransport.SupportsProtocolVersion
// serves that version only when the server is stateless — so an ordinary
// stateful HTTP server negotiates 2025-11-25, and its *server*-side
// middleware sends mast a real elicitation/sampling/roots request instead.
// Disabled does not touch that path. refuseServerInitiatedInput does, so it
// covers both protocol regimes.
//
// Capabilities is set explicitly to the empty set for the same reason.
// The SDK's default advertises roots/listChanged (client.go:262, kept for
// v1.0.0 compatibility) and mast has no roots to offer, so advertising them
// invites a request it is going to refuse.
//
// Supporting elicitation, with the gate wired through it, is a separate and
// larger question. This is only about not having it on by accident.
func newMCPClient() *mcpsdk.Client {
	c := mcpsdk.NewClient(
		&mcpsdk.Implementation{Name: "mast", Version: version.Version},
		&mcpsdk.ClientOptions{
			MultiRoundTrip: &mcpsdk.MultiRoundTripOptions{Disabled: true},
			Capabilities:   &mcpsdk.ClientCapabilities{},
		},
	)
	c.AddReceivingMiddleware(refuseServerInitiatedInput())
	return c
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
//
// Every HTTP server — authenticated or not — gets its transport wrapped
// so a 4xx/5xx carries the server's own error text rather than a bare
// status line; see jsonRPCErrorTransport. The wrap sits *above* auth so
// it observes the response the server actually sent, and it is the
// outermost layer for now: when otelhttp arrives it belongs outside
// this one, so the span records the raw HTTP outcome.
func newHTTPToolset(ctx context.Context, name string, cfg ServerConfig, filter tool.Predicate) (tool.Toolset, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp: server %q: http transport requires a url", name)
	}

	rt := http.DefaultTransport // an http.RoundTripper, so authRT can replace it
	if cfg.Auth != nil && cfg.Auth.GoogleOAuth != nil {
		scopes := cfg.Auth.GoogleOAuth.Scopes
		if len(scopes) == 0 {
			scopes = []string{DefaultGKEScope}
		}
		authRT, err := newGoogleAuthTransport(ctx, name, scopes)
		if err != nil {
			return nil, err
		}
		rt = authRT
	}
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:   cfg.URL,
		HTTPClient: &http.Client{Transport: &jsonRPCErrorTransport{base: rt}},
	}

	ts, err := mcptoolset.New(mcptoolset.Config{
		Client:     newMCPClient(),
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
		Client:     newMCPClient(),
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
