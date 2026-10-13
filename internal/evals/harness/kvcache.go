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
	"context"
	"fmt"
	"time"

	"github.com/go-steer/core-models/kvmetrics"
)

// kvPollInterval spaces the readings inside one incident's run, so the
// peak KV utilization has more than the two endpoints to come from.
const kvPollInterval = 5 * time.Second

// KVWindow is the model server's KV-cache activity while one incident
// ran (JudgeScenario.KVCache), or folded over the corpus
// (JudgeSummary.KVCache): prefix-cache tokens queried and hit, the hit
// rate, and the peak KV utilization.
//
// Fleet-level and time-correlated: the server's whole traffic in the
// window, which in an eval run is normally just this run's. It is
// reported beside usage and never priced. Absent fields were not
// reported by the server.
type KVWindow struct {
	Fleet       bool     `json:"fleet"`
	Queries     *float64 `json:"queries_tokens,omitempty"`
	Hits        *float64 `json:"hits_tokens,omitempty"`
	HitRate     *float64 `json:"hit_rate,omitempty"`
	PeakKVUsage *float64 `json:"peak_kv_usage,omitempty"`
	Readings    int      `json:"readings"`
	Reset       bool     `json:"reset,omitempty"`
	Incidents   int      `json:"incidents,omitempty"`
}

func kvWindowOf(w kvmetrics.Window) *KVWindow {
	return &KVWindow{Fleet: true, Queries: w.Queries, Hits: w.Hits, HitRate: w.HitRate,
		PeakKVUsage: w.PeakKVUsage, Readings: w.Readings, Reset: w.Reset}
}

// fold adds one incident's window to the corpus total.
func (k *KVWindow) fold(w *KVWindow) {
	if w == nil {
		return
	}
	k.Fleet = true
	k.Incidents++
	k.Queries = addKV(k.Queries, w.Queries)
	k.Hits = addKV(k.Hits, w.Hits)
	k.HitRate = nil
	if k.Queries != nil && k.Hits != nil && *k.Queries > 0 {
		r := *k.Hits / *k.Queries
		k.HitRate = &r
	}
	if w.PeakKVUsage != nil && (k.PeakKVUsage == nil || *w.PeakKVUsage > *k.PeakKVUsage) {
		v := *w.PeakKVUsage
		k.PeakKVUsage = &v
	}
	k.Readings += w.Readings
	k.Reset = k.Reset || w.Reset
}

func addKV(a, b *float64) *float64 {
	switch {
	case b == nil:
		return a
	case a == nil:
		v := *b
		return &v
	default:
		v := *a + *b
		return &v
	}
}

// kvRun brackets one incident. A sampler that is nil, or a scrape that
// fails, yields no window and never fails the incident.
type kvRun struct {
	run *kvmetrics.Run
}

func startKV(ctx context.Context, s *kvmetrics.Sampler, note func(string, ...any), warned *bool) kvRun {
	if s == nil {
		return kvRun{}
	}
	run, err := s.Start(ctx, kvPollInterval)
	if err != nil {
		if !*warned {
			*warned = true
			note("[kv] metrics scrape failed, the board will carry no KV-cache figures for the incidents it fails on: %v", err)
		}
		return kvRun{}
	}
	return kvRun{run: run}
}

func (k kvRun) stop(ctx context.Context) *KVWindow {
	if k.run == nil {
		return nil
	}
	w, err := k.run.Stop(ctx)
	if err != nil && w.Readings == 0 {
		return nil
	}
	return kvWindowOf(w)
}

// kvCell renders an incident's hit rate for the board, or "-".
func kvCell(w *KVWindow) string {
	if w == nil || w.HitRate == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *w.HitRate*100)
}

// writeKVCache prints the corpus KV-cache line and each incident's. It
// prints nothing when the model under test publishes no metrics.
func (j *JudgeSummary) writeKVCache(p func(string, ...any)) {
	k := j.KVCache
	if k == nil || k.Incidents == 0 {
		return
	}
	p("")
	p("model server KV cache (fleet-level: the server's whole traffic while each incident ran) — %d incident(s) sampled", k.Incidents)
	line := "  corpus:"
	if k.HitRate != nil {
		line += fmt.Sprintf(" prefix hit rate %.1f%%", *k.HitRate*100)
	}
	if k.Hits != nil && k.Queries != nil {
		line += fmt.Sprintf(" (%.0f of %.0f tokens)", *k.Hits, *k.Queries)
	}
	if k.PeakKVUsage != nil {
		line += fmt.Sprintf(", peak KV %.0f%%", *k.PeakKVUsage*100)
	}
	if k.Reset {
		line += ", server restarted mid-run"
	}
	p("%s", line)
	for _, s := range j.Scenes {
		if s.KVCache != nil {
			p("  %-42s %s", s.ID, kvCell(s.KVCache))
		}
	}
}
