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
	"iter"
	"slices"
	"strings"
	"sync"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/adkv2"
	"github.com/go-steer/core-models/pricing"
	"github.com/go-steer/core-models/profile"
	coreusage "github.com/go-steer/core-models/usage"
	"google.golang.org/adk/v2/model"

	mastusage "github.com/go-steer/mast/internal/providers/usage"
	"github.com/go-steer/mast/internal/taskclass"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/workload"
)

// Provider profiles: every --provider beyond mast's own aliases.
//
// A model outside the Gemini and Claude families is named by
// (profile, model id), not by a prefix (docs/model-support-design.md
// §4.2). Profiles come from go-steer/core-models — its built-ins
// (vertex-maas, ollama, vllm, sglang, openai-compatible) plus whatever
// the operator declares under .agents/providers/ — and are opened
// through it, so the adapter, its retries and its usage record are the
// ones core-agent will run too.
//
// The registry is process-wide because the alias it extends is: every
// function in this package takes the provider as a string, and the CLI
// registers the declared profiles once, at startup, before it
// validates --provider.

var profiles struct {
	mu       sync.RWMutex
	declared []profile.Profile
	opened   map[string]coremodels.Provider
	// prices is the price table built from the profiles in force
	// (pricingTable); nil until first use and after RegisterProfiles.
	prices *priceTable
}

// RegisterProfiles sets the declared provider profiles, replacing any
// registered before. Built-in profiles need no registration.
func RegisterProfiles(ps []profile.Profile) {
	profiles.mu.Lock()
	defer profiles.mu.Unlock()
	profiles.declared = slices.Clone(ps)
	profiles.opened = nil
	profiles.prices = nil
}

// priceTable is what mast prices a call from: core-models' catalog with
// the declared rates as its lowest layer, plus those declared rates on
// their own for the one case the catalog's precedence gets wrong for a
// meter. LiteLLM carries free and preview rows priced at zero, and the
// catalog returns such a row ahead of a lower layer; mast reads a zero
// row as unpriced (catalogPricer.PriceCall says why), so a profile that
// declares a price for that model must still be heard.
type priceTable struct {
	cat *pricing.Catalog
	// declared is the declared layer keyed by the lower-cased
	// "<backend>/<model>", as the catalog keys it.
	declared map[string]pricing.Rates
}

// lookup returns the rates a call to modelID on backend is billed at: a
// catalog row with a price, else the backend's declared row for the
// model, else a miss. A zero row is never returned.
func (t *priceTable) lookup(backend, modelID string) (pricing.Rates, bool) {
	if r, ok := t.cat.LookupFor(backend, modelID); ok && !r.IsZero() {
		return r, true
	}
	if backend != "" {
		if r, ok := t.declared[strings.ToLower(backend+"/"+modelID)]; ok && !r.IsZero() {
			return r, true
		}
	}
	return pricing.Rates{}, false
}

// pricingTable is mast's price table: core-models' compiled-in builtin
// layer, plus the rates provider profiles declare (models[].rates) as
// the lowest layer, keyed "<backend>/<model>". No config override or
// pricing.json file is wired into mast yet, so those layers stay empty.
// A priced catalog row always beats a declared one, so a model a
// published catalog prices is never billed at an operator's guess, and a
// declaration only prices its own profile's backend.
//
// When two profiles declare the same model on the same backend, the
// first wins, in the order profile lookup walks them: built-in profiles
// first, then declared ones in registration order. A declared profile
// that overrides a built-in name takes the built-in's place in that
// order and is counted once. DeclaredFromProfiles keeps the last of
// duplicates, so it is handed the list reversed.
//
// Rebuilt after RegisterProfiles, since declared rates come from the
// profiles registered at startup. Catalog construction with no file
// layers reads nothing, so it cannot fail.
func pricingTable() *priceTable {
	profiles.mu.RLock()
	t := profiles.prices
	profiles.mu.RUnlock()
	if t != nil {
		return t
	}
	profiles.mu.Lock()
	defer profiles.mu.Unlock()
	if profiles.prices != nil {
		return profiles.prices
	}
	t, err := newPriceTable(pricing.Options{Declared: pricing.DeclaredFromProfiles(declaringProfiles(profiles.declared))})
	if err != nil {
		// Unreachable with no file layers, kept explicit so wiring one
		// later can't silently swallow a load error here.
		panic(fmt.Sprintf("compose: pricing catalog: %v", err))
	}
	profiles.prices = t
	return t
}

// newPriceTable builds the catalog for opts and indexes its declared
// layer for priceTable.lookup.
func newPriceTable(opts pricing.Options) (*priceTable, error) {
	c, err := pricing.NewCatalog(opts)
	if err != nil {
		return nil, err
	}
	t := &priceTable{cat: c, declared: make(map[string]pricing.Rates, len(opts.Declared))}
	for k, r := range opts.Declared {
		t.declared[strings.ToLower(k)] = r.Rates()
	}
	return t, nil
}

// pricingCatalog is the catalog half of pricingTable.
func pricingCatalog() *pricing.Catalog { return pricingTable().cat }

// declaringProfiles is every profile that can declare rates, expanded,
// in reverse lookup order (see pricingTable): built-ins first, then
// declared, each name once, with a declared override standing in for
// the built-in it shadows.
func declaringProfiles(declared []profile.Profile) []profile.Profile {
	var out []profile.Profile
	seen := map[string]bool{}
	for _, name := range append(profile.BuiltinNames(), names(declared)...) {
		if seen[name] {
			continue
		}
		seen[name] = true
		p, err := profile.Find(name, declared)
		if err != nil {
			continue
		}
		if p, err = profile.Expand(p); err == nil {
			out = append(out, p)
		}
	}
	slices.Reverse(out)
	return out
}

// mastProvider reports whether provider is one of mast's own aliases
// (or empty, meaning prefix inference), which profiles never shadow.
func mastProvider(provider string) bool {
	switch provider {
	case "", ProviderGemini, ProviderVertex, ProviderAnthropic, ProviderAnthropicVertex, "echo", "scripted":
		return true
	}
	return false
}

// LookupProfile returns the profile a --provider names, extends
// applied, and whether it names one at all. A name that is neither a
// mast alias nor a profile is (zero, false, nil); a profile that exists
// but does not expand is an error.
func LookupProfile(provider string) (profile.Profile, bool, error) {
	if mastProvider(provider) {
		return profile.Profile{}, false, nil
	}
	profiles.mu.RLock()
	declared := profiles.declared
	profiles.mu.RUnlock()
	p, err := profile.Find(provider, declared)
	if err != nil {
		if strings.HasPrefix(err.Error(), "no profile named") {
			return profile.Profile{}, false, nil
		}
		return profile.Profile{}, false, err
	}
	p, err = profile.Expand(p)
	if err != nil {
		return profile.Profile{}, false, err
	}
	return p, true, nil
}

// IsProfileProvider reports whether provider names a provider profile.
func IsProfileProvider(provider string) bool {
	_, ok, _ := LookupProfile(provider)
	return ok
}

// ProfileNames lists every selectable profile: core-models' built-ins
// and the declared ones, sorted.
func ProfileNames() []string {
	profiles.mu.RLock()
	defer profiles.mu.RUnlock()
	names := profile.BuiltinNames()
	for _, p := range profiles.declared {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// openProfile opens a profile once per process for each prompt-cache
// posture. Opening resolves it against the environment — variables,
// credentials — so a missing key fails here, at construction, naming
// the profile (R1).
//
// For an anthropic-dialect profile the cache is keyed on the posture as
// well as the name, because the posture is baked into the opened
// provider (Options.PromptCache), and two bundles in one process (a
// library embed) may ask for different ones.
func openProfile(ctx context.Context, provider string, pc workload.PromptCache) (coremodels.Provider, error) {
	profiles.mu.RLock()
	declared := profiles.declared
	profiles.mu.RUnlock()
	prof, err := profile.Find(provider, declared)
	if err != nil {
		return nil, err
	}
	// The posture is part of the key only where it is part of the
	// provider: the anthropic dialect bakes it in, every other dialect
	// ignores it, and keying those on it would open one provider per
	// posture for nothing.
	key := provider
	if expanded, err := profile.Expand(prof); err == nil && expanded.Dialect == profile.Anthropic && pc.On() {
		key += "\x00cache=" + pc.EffectiveTTL()
	}
	profiles.mu.RLock()
	if p, ok := profiles.opened[key]; ok {
		profiles.mu.RUnlock()
		return p, nil
	}
	profiles.mu.RUnlock()

	// PromptCache is passed explicitly, so the bundle's posture rather
	// than the library's default decides — the two agree today (on, 5
	// minutes), and a bundle that turns caching off must reach the
	// adapter as off. Only the anthropic dialect reads it.
	p, err := coremodels.Open(ctx, prof, coremodels.Options{
		PromptCache: claudeCache(pc),
		Logf:        vendorLogf(provider),
		Resolve:     resolveOptions(profile.Options{}),
	})
	if err != nil {
		return nil, err
	}
	profiles.mu.Lock()
	defer profiles.mu.Unlock()
	if profiles.opened == nil {
		profiles.opened = map[string]coremodels.Provider{}
	}
	profiles.opened[key] = p
	return p, nil
}

// buildProfileModel is BuildModel for a profile-backed provider.
func buildProfileModel(ctx context.Context, provider, name string, mo ModelOptions) (model.LLM, error) {
	bt := mo.BuiltinTools
	// The openai-chat dialect sends function tools only. A bundle that
	// asked for a server-side search or code tool would otherwise run
	// without it and never say so (the shape #340 closed for Gemini).
	if on := builtinsOn(bt); len(on) > 0 {
		return nil, fmt.Errorf("provider profile %q: the bundle turns on server-side built-in tools %v, which this provider cannot run; turn them off in builtin_tools", provider, on)
	}
	p, err := openProfile(ctx, provider, mo.PromptCache)
	if err != nil {
		return nil, err
	}
	m, err := p.Model(ctx, name)
	if err != nil {
		return nil, err
	}
	return usageBridge{inner: adkv2.Wrap(m)}, nil
}

// builtinsOn lists the server-side built-ins a bundle turns on, in the
// neutral vocabulary of builtin_tools.
func builtinsOn(bt workload.BuiltinTools) []string {
	var on []string
	if bt.WebSearchOn() {
		on = append(on, "web_search")
	}
	if bt.URLContextOn() {
		on = append(on, "url_context")
	}
	if bt.CodeExecutionOn() {
		on = append(on, "code_execution")
	}
	return on
}

// usageBridge re-keys core-models' usage record into the sidecar
// mast's meter reads. pkg/budget is frozen at v1.0 and imports nothing
// (#338), so the meter looks for a budget.Detailer under
// budget.DetailKey; core-models attaches its own usage.Detail under its
// own key. The fields are the same record (core-models started from
// mast's #352 shape), so this is a copy, done once per response.
type usageBridge struct {
	inner model.LLM
}

func (b usageBridge) Name() string { return b.inner.Name() }

func (b usageBridge) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for resp, err := range b.inner.GenerateContent(ctx, req, stream) {
			if resp != nil {
				if d, ok := coreusage.FromMetadata(resp.CustomMetadata); ok {
					mastusage.Attach(resp, &mastusage.Detail{
						CacheReadTokens:    d.CacheReadTokens,
						CacheWriteTokens:   d.CacheWriteTokens,
						CacheWrite1hTokens: d.CacheWrite1hTokens,
						ReasoningTokens:    d.ReasoningTokens,
						ToolUseTokens:      d.ToolUseTokens,
						ServedModel:        d.ServedModel,
						ProviderRequestID:  d.ProviderRequestID,
					})
				}
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}

// profileTierModel resolves a tier against a profile's declared tiers.
// Profile tiers extend mast's Go tables rather than outrank them: the
// Gemini and Anthropic aliases keep internal/taskclass, and a profile
// is the only source for its own provider (Resolved decisions, OQ-2).
func profileTierModel(p profile.Profile, tier string) (string, error) {
	if id := p.Tiers[profile.Tier(tier)]; id != "" {
		return id, nil
	}
	return "", fmt.Errorf("provider profile %q declares no %q tier (declare tiers.%s in the profile, or name the model)", p.Name, tier, tier)
}

// ProfileDefaultModel is the model --provider <profile> runs when
// --model is not set: the profile's tier for the task class.
func ProfileDefaultModel(p profile.Profile, tier string) (string, error) {
	if tier == "" {
		tier = taskclass.TierMid
	}
	return profileTierModel(p, tier)
}

// priced reports whether a call to modelName on backend has a price in
// mast's catalog.
func priced(backend, modelName string) bool {
	_, ok := builtinPricer().PriceCall(backend, modelName, budget.Call{UncachedInputTokens: 1000, OutputTokens: 1000})
	return ok
}

func names(ps []profile.Profile) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

// CheckCeilingPriced refuses a cost ceiling on a profile-backed model
// mast cannot price (R6). The meter would price every call at $0 and
// the ceiling would never trip — a ceiling in name only, which is worse
// than none because the operator believes they have one. Gemini and
// Claude keep their flat fallbacks; this applies to profiles, whose
// prices arrive with core-models' catalog (its L6) or as operator-
// declared rates.
func CheckCeilingPriced(scope, provider, modelName string, maxCostUSD float64) error {
	if maxCostUSD <= 0 || IsOfflineFake(modelName) || strings.HasPrefix(modelName, "gemini-") || strings.HasPrefix(modelName, "claude-") {
		return nil
	}
	p, ok, err := LookupProfile(provider)
	if err != nil || !ok {
		return err
	}
	if priced(p.BackendName(), modelName) {
		return nil
	}
	return fmt.Errorf("%s: max_cost_usd %.2f cannot be enforced: model %q on provider profile %q has no price, so every call would cost $0 and the ceiling would never trip; declare its rates in the profile (models[].rates), remove max_cost_usd, or bound the run with max_tokens / max_turns instead",
		scope, maxCostUSD, modelName, p.Name)
}
