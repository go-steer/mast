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
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/adk/v2/model"

	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/taskclass"
)

// Tier resolution — the portable half of a specialist's model override.
//
// `model: claude-haiku-4-5` says which model; `tier: small` says how
// much model the step is worth. The second is the one a shipped bundle
// can declare, because it survives being pointed at another provider:
// the roster says "twelve cheap diagnosers, one mid-tier synthesis" and
// the running provider decides what cheap means. Without it, tiering
// mast's own example bundles would hard-bind them to one vendor, which
// is the objection that kept `model:` off them through v0.3
// (docs/v0.3-plan.md W1.1, finding (b)).
//
// The mapping itself is pkg/taskclass.ModelForTier — the same table
// `--task` already resolves through, so `--task=debug` and
// `tier: frontier` cannot disagree about what the frontier model is.

// providerFamily returns the provider name to resolve a tier against.
//
// An explicit --provider alias wins. With no alias, the family comes
// from the root model id's prefix — the same dispatch BuildModel does
// to decide which client to construct, so tier resolution and model
// construction agree by sharing the question. Both aliases within a
// family map to the same ids in ModelForTier, so deriving "anthropic"
// for a root of `claude-*` running on Vertex is not a mis-resolution:
// anthropicProvider still picks the backend.
func providerFamily(provider, rootModelName string) (string, error) {
	if provider != "" {
		return provider, nil
	}
	switch {
	case strings.HasPrefix(rootModelName, "gemini-"):
		return "gemini", nil
	case strings.HasPrefix(rootModelName, "claude-"):
		return "anthropic", nil
	default:
		return "", fmt.Errorf("cannot tell which provider to resolve a tier against from root model %q (pass --provider)", rootModelName)
	}
}

// TierModelName maps a `tier:` declaration to a concrete model id for
// the running provider. It is the whole of tier resolution; everything
// else in this file is plumbing around it.
//
// An unmappable tier is an error, not a fall-through to the root model.
// A roster that declares tiers and silently runs every specialist on
// the parent's model is the exact fiction W1.1 removed for `model:`.
func TierModelName(provider, rootModelName, tier string) (string, error) {
	family, err := providerFamily(provider, rootModelName)
	if err != nil {
		return "", fmt.Errorf("tier %q: %w", tier, err)
	}
	name := taskclass.ModelForTier(family, tier)
	if name == "" {
		return "", fmt.Errorf("tier %q: no model mapped for provider %q (tiers resolve for gemini, vertex, anthropic and anthropic-vertex)", tier, family)
	}
	return name, nil
}

// SpecModelName is the model id a spec will actually run on, or "" when
// it inherits the parent's. It exists so the two places that need that
// answer — building the agent and pricing its tokens — derive it the
// same way. They did not, at first: MeterScopes priced off `s.Model`
// alone, which would have billed every tiered specialist at the root
// model's rate while the audit log showed the tiered one.
//
// Errors are swallowed to "" on purpose: the callers that price cannot
// act on them, and BuildRoot has already refused the roster by the time
// anything is metered.
func SpecModelName(spec specialists.Spec, provider, rootModelName string) string {
	if spec.Model != "" {
		return spec.Model
	}
	if spec.Tier == "" {
		return ""
	}
	name, err := TierModelName(provider, rootModelName, spec.Tier)
	if err != nil {
		return ""
	}
	return name
}

// NewTierResolver builds the specialists.TierResolver mast ships. It
// resolves through resolve — the memoized ModelResolver — rather than
// calling BuildModel itself, so a roster of twelve small-tier
// diagnosers shares the one client the tier maps to.
//
// Offline fakes collapse before the mapping is attempted, not after:
// under `--model=echo` there is no provider family to derive and
// nothing to tier, and a tiered bundle must still run in the
// credential-free S/U/E test tiers. Same rule NewModelResolver applies
// to `model:` overrides, for the same reason.
func NewTierResolver(ctx context.Context, provider, rootName string, root model.LLM, resolve specialists.ModelResolver, logger *slog.Logger) specialists.TierResolver {
	var warnOnce sync.Once
	return func(tier string) (model.LLM, error) {
		if root != nil && IsOfflineFake(rootName) {
			if logger != nil {
				warnOnce.Do(func() {
					logger.Info("specialist tiers collapsed to the root model: root is an offline fake",
						"root_model", rootName)
				})
			}
			return root, nil
		}
		name, err := TierModelName(provider, rootName, tier)
		if err != nil {
			return nil, err
		}
		if resolve == nil {
			return nil, fmt.Errorf("tier %q resolves to model %q but no model resolver is configured", tier, name)
		}
		return resolve(name)
	}
}

// logTierResolution reports, once per tiered spec at startup, which
// model a tier became. A tier is a claim about spend that the operator
// cannot check by reading the bundle — the answer depends on which
// provider mast was started with — so the resolved id belongs in the
// log next to the roster it applies to.
func logTierResolution(specs []specialists.Spec, provider, rootName string, logger *slog.Logger) {
	if logger == nil || IsOfflineFake(rootName) {
		return
	}
	for _, s := range specs {
		if s.Tier == "" {
			continue
		}
		name, err := TierModelName(provider, rootName, s.Tier)
		if err != nil {
			// BuildRoot surfaces the error itself when it builds this
			// spec; logging a half-answer here would just precede it.
			continue
		}
		logger.Info("specialist tier resolved",
			"specialist", s.Name, "tier", s.Tier, "model", name)
	}
}
