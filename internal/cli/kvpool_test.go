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

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-steer/core-models/kvmetrics"
	"github.com/go-steer/core-models/profile"

	"github.com/go-steer/mast/internal/attach"
)

// climbingVLLM serves a vLLM exposition whose prefix-cache counters
// climb by 100 queried / 40 hit tokens per scrape, with KV usage 0.25.
func climbingVLLM(t *testing.T) *httptest.Server {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := n.Add(1)
		fmt.Fprintf(w, "vllm:prefix_cache_queries_total{engine=\"0\",model_name=\"m\"} %d\n", 100*i)
		fmt.Fprintf(w, "vllm:prefix_cache_hits_total{engine=\"0\",model_name=\"m\"} %d\n", 40*i)
		fmt.Fprintf(w, "vllm:kv_cache_usage_perc{engine=\"0\",model_name=\"m\"} 0.25\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testKVPool(t *testing.T, url string, logs *bytes.Buffer) *kvPool {
	t.Helper()
	s, err := kvmetrics.New(kvmetrics.Options{URL: url, Format: profile.MetricsVLLM})
	if err != nil {
		t.Fatal(err)
	}
	p := newKVPool(s, slog.New(slog.NewTextHandler(logs, nil)))
	p.interval = 0 // endpoints only, so counts are exact
	return p
}

// TestKVPoolFoldsTurns: each turn's window is the counters' increase
// between its two scrapes, the session's is the sum, and the block is
// labelled fleet-level and survives a JSON round trip with nothing
// unreported filled in.
func TestKVPoolFoldsTurns(t *testing.T) {
	var logs bytes.Buffer
	p := testKVPool(t, climbingVLLM(t).URL, &logs)
	ctx := context.Background()
	for range 2 {
		kt := p.begin(ctx)
		p.end("s1", kt)
		p.wait()
	}
	info := p.info("s1")
	if info == nil || !info.Fleet || info.LastTurn == nil || info.Session == nil {
		t.Fatalf("info = %+v, want a fleet block with last turn and session", info)
	}
	if q, h := *info.LastTurn.Queries, *info.LastTurn.Hits; q != 100 || h != 40 {
		t.Errorf("last turn = %v queried / %v hit, want 100 / 40", q, h)
	}
	if s := info.Session; *s.Queries != 200 || *s.Hits != 80 || *s.HitRate != 0.4 || s.Turns != 2 || *s.PeakKVUsage != 0.25 {
		t.Errorf("session = %+v, want 200/80 over 2 turns at 40%%, peak 25%%", s)
	}
	b, _ := json.Marshal(attach.UsageInfo{KVCache: info})
	if !strings.Contains(string(b), `"kv_cache":{"fleet":true`) || strings.Contains(string(b), "hit_rate_gauge") {
		t.Errorf("wire = %s; want the fleet label first and no unreported gauge", b)
	}
	if p.info("other") != nil {
		t.Error("a session with no sampled turn has a kv_cache block")
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected log output: %s", logs.String())
	}
}

// TestKVPoolScrapeFailureIsQuiet: a dead endpoint costs the turn
// nothing, leaves no block, and is logged once per session however many
// turns fail.
func TestKVPoolScrapeFailureIsQuiet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	p := testKVPool(t, srv.URL, &logs)
	for range 3 {
		kt := p.begin(context.Background())
		p.end("s1", kt)
	}
	p.wait()
	if p.info("s1") != nil {
		t.Error("a failed scrape produced a kv_cache block")
	}
	if n := strings.Count(logs.String(), "kv-cache metrics"); n != 1 {
		t.Errorf("%d warnings for three failed turns, want 1:\n%s", n, logs.String())
	}
}

// TestKVPoolNilSamplesNothing: the pool a daemon on a non-profile model
// has is nil, and every call on it is a no-op.
func TestKVPoolNilSamplesNothing(t *testing.T) {
	var p *kvPool
	kt := p.begin(context.Background())
	p.end("s1", kt)
	p.wait()
	if p.info("s1") != nil || newKVPool(nil, nil) != nil {
		t.Error("a nil pool reported something")
	}
}

// TestRenderUsageShowsTheKVBlockAsFleetLevel: the operator view carries
// the section, labelled fleet-level, and leaves out what was not
// reported.
func TestRenderUsageShowsTheKVBlockAsFleetLevel(t *testing.T) {
	rate, peak, q, h := 0.4, 0.25, 200.0, 80.0
	out := attach.RenderUsage(attach.UsageInfo{KVCache: &attach.KVCacheInfo{
		Fleet:   true,
		Session: &attach.KVWindow{Queries: &q, Hits: &h, HitRate: &rate, PeakKVUsage: &peak, Turns: 2},
	}})
	for _, want := range []string{"fleet-level", "prefix hit rate 40.0%", "80 of 200 tokens hit", "peak KV 25%", "2 turn(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gauge") {
		t.Errorf("render shows an unreported gauge:\n%s", out)
	}
}
