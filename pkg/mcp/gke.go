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

// Package mcp wires MCP toolsets for the workloads mast ships. The
// spike scope covers only the gke server (auth via Google OAuth 2.0
// with Application Default Credentials); future servers slot in as
// additional NewXxxToolset constructors.
package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"
)

// DefaultGKEEndpoint is the public GKE MCP server URL.
const DefaultGKEEndpoint = "https://container.googleapis.com/mcp"

// DefaultGKEScope is the OAuth 2.0 scope the GKE MCP server accepts.
// Broad but matches core-agent's recipe and gke-mcp's own docs.
const DefaultGKEScope = "https://www.googleapis.com/auth/cloud-platform"

// GKEConfig configures the GKE MCP toolset.
type GKEConfig struct {
	// Endpoint is the MCP server URL. Defaults to DefaultGKEEndpoint.
	Endpoint string

	// Scopes is the list of OAuth 2.0 scopes to request on the
	// access token. Defaults to []string{DefaultGKEScope}.
	Scopes []string

	// ToolFilter, when non-nil, restricts which of the server's tools
	// are exposed to the agent. Applied at first-fetch time by
	// mcptoolset.
	ToolFilter tool.Predicate

	// Name is a diagnostic label for the toolset. Defaults to "gke".
	Name string
}

// NewGKEToolset builds a tool.Toolset backed by the GKE MCP server,
// authenticated via Google OAuth 2.0 from Application Default
// Credentials. Fails fast if credentials cannot be loaded or an initial
// token cannot be fetched — see newGoogleAuthClient for the exact
// failure modes.
func NewGKEToolset(ctx context.Context, cfg GKEConfig) (tool.Toolset, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = DefaultGKEEndpoint
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{DefaultGKEScope}
	}
	name := cfg.Name
	if name == "" {
		name = "gke"
	}

	httpClient, err := newGoogleAuthClient(ctx, name, scopes)
	if err != nil {
		return nil, err
	}

	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: httpClient,
	}

	ts, err := mcptoolset.New(mcptoolset.Config{
		Transport:  transport,
		ToolFilter: cfg.ToolFilter,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: %q: construct toolset: %w", name, err)
	}
	return ts, nil
}
