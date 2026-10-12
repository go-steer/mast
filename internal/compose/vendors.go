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
	"os"

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

// vendorResolve, when set, is how a built-in profile is resolved against
// the environment. A test seam, beside vendorBaseURL: a test hands the
// ADC-backed profiles a fake token source instead of needing real
// Google credentials.
var vendorResolve *profile.Options

// openVendor opens a core-models built-in profile and returns name as
// an ADK model carrying mast's usage sidecar.
func openVendor(ctx context.Context, builtin, name string, opts coremodels.Options) (model.LLM, error) {
	p, ok := profile.Builtin(builtin)
	if !ok {
		return nil, fmt.Errorf("core-models has no built-in profile %q", builtin)
	}
	return openVendorProfile(ctx, p, name, opts)
}

// openVendorProfile is openVendor for a built-in profile the caller has
// already adjusted (Builtin returns a copy).
//
// opts.Retry is left nil on purpose, which is core-models' retry.Default():
// 429/503 retried at the HTTP layer, and a response-header wait of 5
// minutes for a stream or 10 for an unstreamed call. mast sends every
// model call unstreamed, so the 10-minute bound is the one in force, and
// it is the right one — an unstreamed call receives no headers until the
// whole answer exists. mast's own retry (internal/modelretry) wraps the
// model above this, on ADK errors, and sets no header timeout of its own.
func openVendorProfile(ctx context.Context, p profile.Profile, name string, opts coremodels.Options) (model.LLM, error) {
	if vendorBaseURL != "" {
		p.BaseURL = vendorBaseURL
	}
	if vendorResolve != nil {
		opts.Resolve = *vendorResolve
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
	opts := coremodels.Options{BuiltinTools: builtinsOn(bt), Logf: vendorLogf("gemini")}
	if !geminiOnVertex(provider) {
		return openVendor(ctx, ProviderGemini, name, opts)
	}
	if vertexExpress(provider) {
		p, _ := profile.Builtin(ProviderVertexExpress)
		// genai's express-mode key set and order: GOOGLE_API_KEY, then
		// GEMINI_API_KEY. The profile reads only the first.
		p.Auth.AltEnv = []string{"GEMINI_API_KEY"}
		return openVendorProfile(ctx, p, name, opts)
	}
	// Checked here for its error, which names the variable to set; the
	// built-in vertex profile resolves the same project and location
	// chain (GOOGLE_CLOUD_LOCATION, then GOOGLE_CLOUD_REGION, then
	// global) itself.
	if _, err := geminiClientConfig(ProviderVertex); err != nil {
		return nil, err
	}
	return openVendor(ctx, ProviderVertex, name, opts)
}

// buildClaude serves a claude-* model on the backend anthropicBackend
// resolves.
//
// Two postures are mast's and are passed explicitly. Prompt caching
// follows the bundle's prompt_cache block (see claudeCache). And only
// web_search reaches Claude: url_context and code_execution have no
// Anthropic equivalent, which mast always treated as fail-safe (the
// tool cannot be on), where the library refuses the request to send
// them.
func buildClaude(ctx context.Context, provider, name string, mo ModelOptions) (model.LLM, error) {
	backend, err := anthropicBackend(provider)
	if err != nil {
		return nil, err
	}
	return openVendor(ctx, backend, name, coremodels.Options{
		BuiltinTools: anthropicBuiltins(mo.BuiltinTools),
		PromptCache:  claudeCache(mo.PromptCache),
		Logf:         vendorLogf("anthropic"),
	})
}

// anthropicBuiltins is builtinsOn narrowed to the built-ins the
// Anthropic adapter has, asked of the adapter itself so the two lists
// cannot drift. A name it lacks is dropped rather than sent: mast has
// always treated a built-in with no equivalent as off, where the
// library refuses the request.
func anthropicBuiltins(bt workload.BuiltinTools) []string {
	var out []string
	for _, n := range builtinsOn(bt) {
		if _, err := anthropic.BuiltinToolsFromNames([]string{n}); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// vendorLogf routes an adapter's operator-facing notices — a pre-3.0
// Gemini model running without its built-ins, an empty response
// retried or persisting, a context cache evicted — to the daemon's
// stderr in the line format mast's own adapters used ("mast: gemini:
// ..."). Package-level so a test can intercept it.
var vendorLogf = func(family string) func(format string, args ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "mast: "+family+": "+format+"\n", args...)
	}
}

// ProviderVertexExpress is core-models' built-in profile for Vertex AI
// express mode: Gemini on Vertex with an API key and no project.
const ProviderVertexExpress = "vertex-express"

// vertexExpress reports whether a gemini-* model runs in Vertex AI
// express mode — exactly when genai's own env selection chose it before
// mast moved to core-models: no alias naming the backend outright (the
// `vertex` alias is ADC by design), GOOGLE_GENAI_USE_VERTEXAI set, an API
// key, and no project or location in the environment. genai lets an
// implicit project or location win over an implicit key, and so does
// this.
func vertexExpress(provider string) bool {
	if provider == ProviderVertex || !geminiOnVertex(provider) {
		return false
	}
	for _, v := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"} {
		if os.Getenv(v) != "" {
			return false
		}
	}
	return os.Getenv("GOOGLE_API_KEY") != "" || os.Getenv("GEMINI_API_KEY") != ""
}

// claudeCache maps the bundle's prompt_cache block onto the adapter's
// breakpoints: history plus system on the bundle's TTL, or none at all.
// Never nil, because nil means the library's own default — and what a
// workload caches is the bundle's to say.
func claudeCache(pc workload.PromptCache) *anthropic.CacheOptions {
	if !pc.On() {
		return &anthropic.CacheOptions{}
	}
	ttl := anthropic.TTL5m
	if pc.EffectiveTTL() == workload.PromptCacheTTL1h {
		ttl = anthropic.TTL1h
	}
	return &anthropic.CacheOptions{System: true, History: true, TTL: ttl}
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
