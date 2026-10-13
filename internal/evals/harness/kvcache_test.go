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

package harness

import (
	"fmt"
	"strings"
	"testing"
)

func kvw(q, h, peak float64) *KVWindow {
	r := h / q
	return &KVWindow{Fleet: true, Queries: &q, Hits: &h, HitRate: &r, PeakKVUsage: &peak, Readings: 2}
}

// TestKVCacheBoard: the corpus line folds every sampled incident (counts
// summed, rate recomputed, peak the highest), each incident gets its own
// row, and a board with nothing sampled prints nothing.
func TestKVCacheBoard(t *testing.T) {
	j := &JudgeSummary{Scenes: []JudgeScenario{
		{ID: "LC-01", KVCache: kvw(100, 40, 0.2)},
		{ID: "LC-06", KVCache: kvw(300, 0, 0.5)},
		{ID: "LC-10"},
	}}
	j.KVCache = &KVWindow{}
	for _, s := range j.Scenes {
		j.KVCache.fold(s.KVCache)
	}
	if k := j.KVCache; k.Incidents != 2 || *k.Queries != 400 || *k.Hits != 40 || *k.HitRate != 0.1 || *k.PeakKVUsage != 0.5 {
		t.Errorf("corpus fold = %+v", k)
	}
	var out strings.Builder
	j.writeKVCache(func(f string, a ...any) { fmt.Fprintf(&out, f+"\n", a...) })
	for _, want := range []string{"fleet-level", "2 incident(s) sampled", "prefix hit rate 10.0%", "(40 of 400 tokens)", "peak KV 50%", "LC-01", "40%", "LC-06"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("board missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "LC-10") {
		t.Errorf("an unsampled incident got a row:\n%s", out.String())
	}

	var empty strings.Builder
	(&JudgeSummary{}).writeKVCache(func(f string, a ...any) { fmt.Fprintf(&empty, f+"\n", a...) })
	if empty.Len() != 0 {
		t.Errorf("a board with no sampler printed:\n%s", empty.String())
	}
}
