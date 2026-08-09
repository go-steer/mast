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

// Command mcpserver is a minimal stdio MCP server used only by
// pkg/mcp's tests to exercise the local-process (stdio) transport path
// end to end. It exposes a single "ping" tool that returns "pong". The
// name it reports is overridable via MCP_TEST_TOOL_NAME so a test can
// assert that env passed through buildStdioCommand reaches the child.
package main

import (
	"context"
	"os"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	toolName := os.Getenv("MCP_TEST_TOOL_NAME")
	if toolName == "" {
		toolName = "ping"
	}

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "mast-test-mcp", Version: "0.0.1"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: toolName, Description: "returns pong"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "pong"}},
			}, struct{}{}, nil
		})

	if err := srv.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}
