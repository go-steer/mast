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

// Package gemini wraps an ADK Gemini model.LLM with the behavior mast
// needs for unattended operation: server-side built-in tool injection
// (Google Search / URL Context / Code Execution), Vertex explicit
// context-cache stamping, empty-response detection + retry, and
// cache-eviction recovery.
//
// Unlike its core-agent ancestor, this package does NOT construct the
// base LLM and carries no provider registry or config coupling — mast
// builds the base model itself (see internal/compose) and hands it to
// Wrap along with an Options struct describing the backend it fronts.
package gemini

import (
	adkmodel "google.golang.org/adk/v2/model"
)

// Options configures the wrapper returned by Wrap. The zero value is
// valid: no built-ins injected, no cache hooks, no backend-specific
// flags — the wrapper still contributes the empty-response detection
// + retry-once safety net (#220 / #78).
type Options struct {
	// BuiltinTools toggles Gemini's server-side built-in tools. Each
	// enabled flag becomes its own *genai.Tool entry appended to the
	// request's Config.Tools alongside any user-defined function
	// declarations. Zero value = no built-ins; pass
	// DefaultBuiltinTools() for the standard GoogleSearch + URLContext
	// baseline.
	BuiltinTools BuiltinTools

	// IncludeServerSideToolInvocations must be set when the wrapper
	// fronts the direct Gemini API (genai.BackendGeminiAPI) and
	// built-ins ride alongside function tools — Gemini 3+ rejects the
	// combination without the flag. Leave false on Vertex AI, which
	// rejects the parameter outright ("includeServerSideToolInvocations
	// parameter is not supported in Gemini Enterprise Agent Platform
	// (previously known as Vertex AI)") but permits the combination
	// unconditionally instead.
	IncludeServerSideToolInvocations bool

	// TolerateEmptyChunks swallows ADK's per-chunk "empty response"
	// errors on streaming requests. Vertex's streaming search-grounding
	// path intermittently emits chunks with empty Candidates[]
	// (heartbeat-like, carrying only UsageMetadata/ResponseID); ADK can
	// surface these as fatal errors that poison the stream before the
	// grounded chunks arrive. Set on Vertex; leave false on the direct
	// Gemini API to preserve real "no content" failure signaling.
	TolerateEmptyChunks bool

	// ContextCacheInit + ContextCacheName wire Vertex explicit context
	// caching (#221). Init captures the fully-assembled system
	// instruction + tools on the first uncached call (stamp side);
	// Name returns the resolved cache handle to stamp onto
	// GenerateContentConfig.CachedContent, or "" to run uncached
	// (strip side degrades gracefully). Both nil = no caching. Only
	// meaningful on the Vertex backend — the direct Gemini API rejects
	// the cache-reference parameter on some model families.
	ContextCacheInit ContextCacheInitFn
	ContextCacheName ContextCacheNameFn

	// ContextCacheInvalidate is the eviction-recovery hook — called
	// when GenerateContent detects that Vertex has reaped the cache
	// server-side (NOT_FOUND on the stamped reference). See
	// ContextCacheInvalidateFn. Optional: without it, this-turn retry
	// still fires but the manager isn't reset, so later turns keep
	// attempting the dead handle until an external reset.
	ContextCacheInvalidate ContextCacheInvalidateFn
}

// Wrap returns base wrapped with mast's Gemini behavior layer per
// opts. The returned LLM is stateless and safe for concurrent use.
//
// Wrap always wraps — even with a zero Options the result carries the
// empty-response detection + retry-once safety nets, which are
// backend-independent. Callers that need the raw base model back
// (e.g. a subtask path that must pass EXACTLY its own tool set) can
// duck-type for the WithoutBuiltins() method — see
// (*builtinsLLM).WithoutBuiltins.
func Wrap(base adkmodel.LLM, opts Options) adkmodel.LLM {
	return &builtinsLLM{
		inner:               base,
		builtins:            opts.BuiltinTools.asTools(),
		isDirectGeminiAPI:   opts.IncludeServerSideToolInvocations,
		tolerateEmptyChunks: opts.TolerateEmptyChunks,
		cacheInit:           opts.ContextCacheInit,
		cacheName:           opts.ContextCacheName,
		cacheInvalidate:     opts.ContextCacheInvalidate,
	}
}
