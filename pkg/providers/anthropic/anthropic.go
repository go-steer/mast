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

// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818

// Package anthropic adapts Anthropic / Claude to the ADK's model.LLM
// interface.
//
// ADK Go ships only the Gemini and Apigee model backends, so this
// package adapts the official Anthropic Go SDK
// (github.com/anthropics/anthropic-sdk-go) to the ADK's model.LLM
// interface. genai-shaped requests are translated to Anthropic's
// Messages API; streaming responses are accumulated back into
// genai-shaped events the ADK runner expects.
//
// Conversation history is preserved automatically by the ADK runner
// (the in-memory session service replays prior events on each turn);
// this provider is stateless aside from the API client.
//
// Unlike its core-agent ancestor, this package registers nothing at
// init time and reads no central config: callers construct a Provider
// explicitly via New (first-party API) or NewVertex (Anthropic on
// Vertex AI) with an Options / VertexOptions struct.
package anthropic

import (
	"context"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	adkmodel "google.golang.org/adk/v2/model"
)

// Provider identities reported by Name(), so telemetry can tell the
// first-party and Vertex backends apart.
const (
	ProviderName       = "anthropic"
	VertexProviderName = "anthropic-vertex"
)

// DefaultModel is used when LLMRequest.Model is empty. We follow the
// claude-api skill's guidance and default to the most capable Opus.
const DefaultModel = "claude-opus-4-7"

// DefaultSmallModelID is the Anthropic cheap-tier model used by default
// for agentic subtasks when the operator hasn't pinned one. Same value
// for the first-party and Vertex backends; the Vertex publication name
// resolves at call time.
const DefaultSmallModelID = "claude-haiku-4-5"

// DefaultMaxTokens caps a single response when the caller hasn't set
// one. 16K is a comfortable middle ground: plenty for most turns,
// well under the streaming SDK's HTTP timeouts.
const DefaultMaxTokens = 16_384

// EnvAPIKey is the environment variable consulted when no key is
// supplied via Options.
const EnvAPIKey = "ANTHROPIC_API_KEY" // #nosec G101 -- env var name, not a credential

// Provider is the Anthropic model provider. The same struct serves
// both the first-party API and Vertex AI backends — only the embedded
// client differs. name carries which one this is so telemetry sees the
// right identity.
type Provider struct {
	name        string
	client      anthropic.Client
	cacheSystem bool
	builtins    BuiltinTools
}

// Options configures New. The zero value plus an API key (or the
// ANTHROPIC_API_KEY env var) is a working configuration.
type Options struct {
	// APIKey authenticates against the first-party api.anthropic.com.
	// Empty falls back to the ANTHROPIC_API_KEY env var; a set field
	// always wins over env.
	APIKey string

	// CacheSystem enables prompt caching on the last system block.
	// Off by default — turn it on once you've confirmed the system
	// prompt is stable across turns (otherwise the cache write premium
	// is paid for nothing).
	CacheSystem bool

	// BuiltinTools toggles Anthropic's server-side built-in tools
	// (e.g. web_search). The zero value — everything off — matches
	// DefaultBuiltinTools(); see the BuiltinTools doc for why off is
	// the default.
	BuiltinTools BuiltinTools
}

// New constructs a Provider for the first-party api.anthropic.com.
// An empty Options.APIKey falls back to the ANTHROPIC_API_KEY env var.
func New(opts Options) (*Provider, error) {
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = os.Getenv(EnvAPIKey)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("anthropic: api key is required (set Options.APIKey or the %s env var)", EnvAPIKey)
	}
	return &Provider{
		name:        ProviderName,
		client:      anthropic.NewClient(option.WithAPIKey(apiKey)),
		cacheSystem: opts.CacheSystem,
		builtins:    opts.BuiltinTools,
	}, nil
}

// Name reports the provider identity ("anthropic" or "anthropic-vertex").
func (p *Provider) Name() string { return p.name }

// DefaultSmallModel reports the cheap-tier Claude model a caller should
// route subtask digesting to when the operator hasn't pinned one.
func (p *Provider) DefaultSmallModel() string { return DefaultSmallModelID }

// Model returns a model.LLM for the given model ID. modelID may be
// empty, in which case DefaultModel is used.
//
// Note: Vertex AI sometimes serves Claude under date-suffixed model IDs
// (e.g. "claude-opus-4-5@20251101"). When using the Vertex backend,
// pass the exact ID Vertex expects; the SDK plugs it into the Vertex
// URL path verbatim.
func (p *Provider) Model(_ context.Context, modelID string) (adkmodel.LLM, error) {
	if modelID == "" {
		modelID = DefaultModel
	}
	return &llm{
		client:      p.client,
		modelID:     modelID,
		cacheSystem: p.cacheSystem,
		builtins:    p.builtins,
	}, nil
}
