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

package compose

import (
	"context"
	"fmt"
	"maps"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/adkv2"
	"github.com/go-steer/core-models/dialect/anthropic"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/profile"
	"google.golang.org/adk/v2/model"

	"github.com/go-steer/mast/pkg/workload"
)

// Gemini and Claude through core-models (L4/L5).
//
// mast's own adapters for both families moved into the library; what
// stays here is product policy: which backend an alias plus the
// environment selects, the built-in tool posture (#340), and the
// prompt-cache posture. The library's built-in profiles carry the env
// chains mast already used, so a profile is opened rather than a
// dialect client built by hand — one construction path for every
// provider, the same one --provider <profile> takes.

// The Anthropic provider aliases and defaults, formerly exported by
// mast's internal/providers/anthropic. The alias strings are also the
// core-models built-in profile names and the pricing backends.
const (
	ProviderAnthropic       = "anthropic"
	ProviderAnthropicVertex = "anthropic-vertex"

	// EnvAnthropicAPIKey selects the first-party API when no alias does.
	EnvAnthropicAPIKey = "ANTHROPIC_API_KEY" // #nosec G101 -- env var name, not a credential
	// EnvAnthropicVertexProject selects Claude on Vertex when no alias
	// does; GOOGLE_CLOUD_PROJECT does too.
	EnvAnthropicVertexProject = "ANTHROPIC_VERTEX_PROJECT_ID"

	// DefaultAnthropicModel is the claude-* model an Anthropic alias runs
	// when nothing names one.
	DefaultAnthropicModel = "claude-opus-5"
	// DefaultAnthropicSmallModel is the Anthropic small tier.
	DefaultAnthropicSmallModel = "claude-haiku-4-5"
)

// vendorBaseURL, when set, replaces a built-in profile's endpoint. A
// test seam: it is how a test points the Gemini and Claude paths at a
// recorder and reads back what mast actually puts on the wire.
var vendorBaseURL string

// openVendor opens a core-models built-in profile, after mutate (if
// any) adjusts a copy of it, and returns name as an ADK model carrying
// mast's usage sidecar.
func openVendor(ctx context.Context, builtin, name string, mutate func(*profile.Profile), opts coremodels.Options) (model.LLM, error) {
	p, ok := profile.Builtin(builtin)
	if !ok {
		return nil, fmt.Errorf("core-models has no built-in profile %q", builtin)
	}
	if mutate != nil {
		p.Params = maps.Clone(p.Params)
		mutate(&p)
	}
	if vendorBaseURL != "" {
		p.BaseURL = vendorBaseURL
	}
	prov, err := coremodels.Open(ctx, p, opts)
	if err != nil {
		return nil, err
	}
	m, err := prov.Model(ctx, name)
	if err != nil {
		return nil, err
	}
	return bridged(m), nil
}

// buildGemini serves a gemini-* model: Vertex AI when geminiOnVertex
// says so, otherwise the Developer API keyed from GOOGLE_API_KEY or
// GEMINI_API_KEY — genai's own env selection, restated.
func buildGemini(ctx context.Context, provider, name string, bt workload.BuiltinTools) (model.LLM, error) {
	opts := coremodels.Options{BuiltinTools: builtinsOn(bt)}
	if !geminiOnVertex(provider) {
		return openVendor(ctx, ProviderGemini, name, nil, opts)
	}
	cfg, err := geminiClientConfig(ProviderVertex)
	if err != nil {
		return nil, err
	}
	// mast's location chain (GOOGLE_CLOUD_LOCATION, GOOGLE_CLOUD_REGION,
	// global) is one variable longer than the profile's; pin both
	// params to what mast resolved so the two cannot disagree.
	return openVendor(ctx, ProviderVertex, name, func(p *profile.Profile) {
		p.Params["project"] = cfg.Project
		p.Params["region"] = cfg.Location
	}, opts)
}

// buildClaude serves a claude-* model on the backend anthropicBackend
// resolves.
//
// Two postures are mast's and are passed explicitly. Prompt caching
// stays off — no breakpoints — as mast's adapter shipped it; the
// library defaults it on, and turning it on is a measured decision of
// its own. And only web_search reaches Claude: url_context and
// code_execution have no Anthropic equivalent, which mast always
// treated as fail-safe (the tool cannot be on), where the library
// refuses the request to send them.
func buildClaude(ctx context.Context, provider, name string, bt workload.BuiltinTools) (model.LLM, error) {
	backend, err := anthropicBackend(provider)
	if err != nil {
		return nil, err
	}
	var builtins []string
	if bt.WebSearchOn() {
		builtins = []string{"web_search"}
	}
	return openVendor(ctx, backend, name, nil, coremodels.Options{
		BuiltinTools: builtins,
		PromptCache:  &anthropic.CacheOptions{},
	})
}

// bridged adapts a core-models model to ADK with mast's usage sidecar,
// keeping its built-in tool report visible to BuiltinToolsSummary.
func bridged(m llm.LLM) model.LLM {
	b := usageBridge{inner: adkv2.Wrap(m)}
	if r, ok := m.(BuiltinToolsReporter); ok {
		return reportingBridge{usageBridge: b, report: r}
	}
	return b
}

type reportingBridge struct {
	usageBridge
	report BuiltinToolsReporter
}

func (r reportingBridge) BuiltinToolNames() []string { return r.report.BuiltinToolNames() }
