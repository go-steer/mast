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

package toolcatalog

import (
	"context"
	"net/http"
	"net/http/httptest"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// StartStubMCP starts an in-process streamable-HTTP MCP server and
// returns its endpoint plus a stop function. No network, no external
// process: the caller passes the endpoint to Build as Config.MCPEndpoint.
//
// The two tools are shaped to be awkward on purpose. A converter that
// only ever sees {"type":"string"} properties can drop a lot and still
// look fine; these carry a required/optional split, an enum, an array
// of objects, and a nested object, which is what a real workload's MCP
// server advertises and what an adapter has to carry to the wire
// unmangled.
//
// It lives here rather than in a test file because two provider test
// packages need the same fixture and neither should own it — and
// keeping it free of *testing.T is what lets it be shared at all.
func StartStubMCP() (endpoint string, stop func()) {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "mast-toolcatalog-stub",
		Version: "0.0.1",
	}, nil)

	// mcpsdk.Tool.InputSchema is `any` and travels as raw JSON, so the
	// fixtures are written as the JSON a server actually advertises
	// rather than through a Go schema type that might normalize
	// something on the way out.
	addStubTool(srv, &mcpsdk.Tool{
		Name:        "stub_search",
		Description: "Search the stub corpus. Required query, optional bounded limit.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "What to search for."},
				"limit": map[string]any{"type": "integer", "description": "Maximum results."},
				"scope": map[string]any{"type": "string", "enum": []any{"repo", "org", "global"}},
			},
			"required": []any{"query"},
		},
	})

	addStubTool(srv, &mcpsdk.Tool{
		Name:        "stub_apply_patch",
		Description: "Apply edits. Nested object and array-of-object arguments.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
						"rev":  map[string]any{"type": "string"},
					},
					"required": []any{"path"},
				},
				"edits": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"old": map[string]any{"type": "string"},
							"new": map[string]any{"type": "string"},
						},
						"required": []any{"old", "new"},
					},
				},
				"dry_run": map[string]any{"type": "boolean"},
			},
			"required": []any{"target", "edits"},
		},
	})

	hs := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv }, nil))
	// ADK's connectionRefresher never closes the MCP session it opens,
	// so the SSE stream outlives the caller and a plain Close blocks on
	// it — same wrinkle pkg/mcp's own fixtures work around.
	return hs.URL, func() {
		hs.CloseClientConnections()
		hs.Close()
	}
}

// addStubTool registers a tool whose handler is never reached: the
// catalog's recording model enumerates declarations and stops, so
// nothing in this package ever issues a call.
func addStubTool(srv *mcpsdk.Server, t *mcpsdk.Tool) {
	srv.AddTool(t, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "stub"}},
		}, nil
	})
}
