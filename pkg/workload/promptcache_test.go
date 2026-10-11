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

package workload_test

import (
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/workload"
)

// TestLoad_PromptCache: absent is on with the 5-minute TTL (the
// default, a breaking change from the release before the block),
// enabled: false turns it off, each field loads as written, and a typo'd TTL is a refused bundle naming the file
// rather than a cache that quietly runs on the default.
func TestLoad_PromptCache(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml    string
		on      bool
		ttl     string
		wantErr string
	}{
		"absent":    {yaml: "", on: true, ttl: workload.PromptCacheTTL5m},
		"off":       {yaml: "prompt_cache:\n  enabled: false\n", on: false, ttl: workload.PromptCacheTTL5m},
		"ttl alone": {yaml: "prompt_cache:\n  ttl: 1h\n", on: true, ttl: workload.PromptCacheTTL1h},
		"one hour":  {yaml: "prompt_cache:\n  enabled: true\n  ttl: 1h\n", on: true, ttl: workload.PromptCacheTTL1h},
		"5m":        {yaml: "prompt_cache:\n  enabled: true\n  ttl: 5m\n", on: true, ttl: workload.PromptCacheTTL5m},
		"typo":      {yaml: "prompt_cache:\n  ttl: 60m\n", wantErr: `unknown prompt_cache.ttl "60m"`},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\n"+tc.yaml)
			b, err := workload.Load(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "b.yaml") {
					t.Fatalf("Load = %v, want an error naming b.yaml and containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if b.PromptCache.On() != tc.on || b.PromptCache.EffectiveTTL() != tc.ttl {
				t.Errorf("PromptCache = on %v ttl %q, want on %v ttl %q",
					b.PromptCache.On(), b.PromptCache.EffectiveTTL(), tc.on, tc.ttl)
			}
		})
	}
}
