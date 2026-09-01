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

// Originally derived from go-steer/core-agent@cafe3106cf61cb7c1edbb39c2ce446dd87358747

package pricing

// Options bundle the inputs NewCatalog needs to assemble the
// layered view. All fields are optional — an empty Options yields
// a catalog with only the compiled-in builtin layer (useful for
// tests + as the default before main.go wires the files in).
type Options struct {
	// CfgOverride is the operator's per-model override from
	// .agents/config.json's `model.pricing` map. Highest precedence.
	CfgOverride map[string]ModelRates

	// AgentsDir is the resolved .agents/ directory (empty when no
	// project root was found). Catalog construction reads
	// <agentsDir>/pricing.json if present.
	AgentsDir string

	// UserHome is the per-user mast state directory (usually
	// ~/.mast). Catalog construction reads <UserHome>/pricing.json
	// if present (both manual + external sections).
	UserHome string
}

// NewCatalog reads every configured source and returns the merged
// catalog. Missing files are not errors (the common case); only
// I/O failures and malformed JSON return non-nil.
//
// The returned catalog is read-only after construction. Callers
// that want a refresh (the daily LiteLLM fetch, or after /reload) build
// a new Catalog and swap atomically via the consumer's chosen
// pointer-store mechanism.
func NewCatalog(opts Options) (*Catalog, error) {
	// The two generated tables share one layer. Their key spaces cannot
	// collide — a qualified key always contains "/" and a bare model id
	// never does — and merging them here means the layered precedence
	// chain and the prefix fallback work on qualified keys without
	// knowing they exist.
	c := &Catalog{
		cfgOverride: lowerKeys(opts.CfgOverride),
		builtin:     lowercopyRates(builtin, builtinByBackend),
	}

	if opts.AgentsDir != "" {
		pf, err := LoadProjectFile(opts.AgentsDir)
		if err != nil {
			return nil, err
		}
		c.projectFile = lowerKeys(pf.Models)
	}

	if opts.UserHome != "" {
		uf, err := LoadUserFile(opts.UserHome)
		if err != nil {
			return nil, err
		}
		if uf.Manual != nil {
			c.userManual = lowerKeys(uf.Manual.Models)
		}
		if uf.External != nil {
			c.userExt = lowerKeys(uf.External.Models)
		}
	}

	return c, nil
}

// lowercopyRates clones already-lowercased Rates maps into one (used
// for the generated tables, which are lowercase by construction but
// should never be mutated by lookups or tests). Later maps win on a
// duplicate key; the generated tables have none.
func lowercopyRates(srcs ...map[string]Rates) map[string]Rates {
	n := 0
	for _, s := range srcs {
		n += len(s)
	}
	if n == 0 {
		return nil
	}
	out := make(map[string]Rates, n)
	for _, s := range srcs {
		for k, v := range s {
			out[k] = v
		}
	}
	return out
}
