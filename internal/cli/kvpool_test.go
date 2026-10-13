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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/core-models/kvmetrics"
	"github.com/go-steer/core-models/profile"

	"github.com/go-steer/mast/internal/attach"
	"github.com/go-steer/mast/internal/kvwindow"
)

// climbingVLLM serves a vLLM exposition whose prefix-cache counters
// climb by 100 queried / 40 hit tokens per scrape, with KV usage 0.25,
// and counts the scrapes.
func climbingVLLM(t *testing.T, scrapes *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := scrapes.Add(1)
		fmt.Fprintf(w, "vllm:prefix_cache_queries_total{engine=\"0\",model_name=\"m\"} %d\n", 100*i)
		fmt.Fprintf(w, "vllm:prefix_cache_hits_total{engine=\"0\",model_name=\"m\"} %d\n", 40*i)
		fmt.Fprintf(w, "vllm:kv_cache_usage_perc{engine=\"0\",model_name=\"m\"} 0.25\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testKVPool is a pool on url with the poller effectively off (poll is
// how often it scrapes; an hour means only edges scrape, so counts are
// exact).
func testKVPool(t *testing.T, url string, poll time.Duration, logs *bytes.Buffer) *kvPool {
	t.Helper()
	p := newKVPool(context.Background(), "lab", "m", slog.New(slog.NewTextHandler(logs, nil)))
	p.sampler = func(context.Context) (*kvmetrics.Sampler, error) {
		return kvmetrics.New(kvmetrics.Options{URL: url, Format: profile.MetricsVLLM})
	}
	p.interval = poll
	t.Cleanup(p.stop)
	return p
}

// TestKVPoolRecordsTurnsInOrderWithoutDoubleCounting: consecutive turns'
// windows abut — the second's start edge is the first's end reading —
// so the session sums each token once, and last_turn is the latest
// turn's.
func TestKVPoolRecordsTurnsInOrderWithoutDoubleCounting(t *testing.T) {
	var scrapes atomic.Int64
	var logs bytes.Buffer
	p := testKVPool(t, climbingVLLM(t, &scrapes).URL, time.Hour, &logs)
	for range 2 {
		p.end("s1", p.begin())
	}
	info := p.info("s1")
	if info == nil || !info.Fleet || info.LastTurn == nil || info.Session == nil {
		t.Fatalf("info = %+v, want a fleet block with last turn and session", info)
	}
	if q, h := *info.LastTurn.Queries, *info.LastTurn.Hits; q != 100 || h != 40 {
		t.Errorf("last turn = %v / %v, want 100 / 40", q, h)
	}
	s := info.Session
	if *s.Queries != 200 || *s.Hits != 80 || *s.HitRate != 0.4 || s.Windows != 2 || *s.PeakKVUsage != 0.25 {
		t.Errorf("session = %+v, want 200/80 over 2 turns at 40%%, peak 25%%", s)
	}
	if s.HitRateGauge != nil {
		t.Error("the session fold carries an instantaneous gauge")
	}
	if n := scrapes.Load(); n != 3 {
		t.Errorf("%d scrapes for two abutting turns, want 3 (the end edge doubles as the next start)", n)
	}
	b, _ := json.Marshal(attach.UsageInfo{KVCache: info})
	if !strings.Contains(string(b), `"kv_cache":{"fleet":true`) || !strings.Contains(string(b), `"windows":2`) {
		t.Errorf("wire = %s", b)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("unexpected warning: %s", logs.String())
	}
}

// TestKVPoolOnePollerForManySessions: concurrent turns in several
// sessions share one poller, and stop ends it.
func TestKVPoolOnePollerForManySessions(t *testing.T) {
	var scrapes atomic.Int64
	var logs bytes.Buffer
	p := testKVPool(t, climbingVLLM(t, &scrapes).URL, 10*time.Millisecond, &logs)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kt := p.begin()
			time.Sleep(60 * time.Millisecond)
			p.end(fmt.Sprintf("s%d", i), kt)
		}()
	}
	wg.Wait()
	if p.pollers != 1 {
		t.Errorf("%d pollers for three sessions, want 1", p.pollers)
	}
	for i := range 3 {
		if w := p.info(fmt.Sprintf("s%d", i)); w == nil || w.LastTurn.Readings < 3 {
			t.Errorf("session s%d window = %+v, want polled readings between the edges", i, w)
		}
	}
	p.stop()
	before := scrapes.Load()
	time.Sleep(40 * time.Millisecond)
	if after := scrapes.Load(); after != before {
		t.Errorf("the poller scraped %d more times after stop", after-before)
	}
}

// TestKVPoolDeadEndpointIsBoundedAndQuiet: a failing endpoint costs a
// turn at most one bounded edge scrape, then nothing while backed off;
// no window is recorded and one warning is logged.
func TestKVPoolDeadEndpointIsBoundedAndQuiet(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	p := testKVPool(t, srv.URL, time.Hour, &logs)
	p.edgeTimeout = 50 * time.Millisecond
	start := time.Now()
	for range 3 {
		p.end("s1", p.begin())
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("three turns spent %v on a dead endpoint, want one bounded edge scrape", d)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("%d scrapes against a backed-off endpoint, want 1", n)
	}
	if p.info("s1") != nil {
		t.Error("a dead endpoint produced a kv_cache block")
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, logs.String())
	}
}

// TestKVPoolNothingToSample: a nil pool, and a pool whose model
// publishes no metrics, sample nothing and start no poller.
func TestKVPoolNothingToSample(t *testing.T) {
	var p *kvPool
	p.end("s1", p.begin())
	p.stop()
	if p.info("s1") != nil {
		t.Error("a nil pool reported something")
	}
	q := newKVPool(context.Background(), "vertex", "gemini-3.5-flash", nil)
	q.sampler = func(context.Context) (*kvmetrics.Sampler, error) { return nil, nil }
	q.end("s1", q.begin())
	q.stop()
	if q.pollers != 0 || q.info("s1") != nil {
		t.Errorf("a pool with no sampler started %d pollers / reported %+v", q.pollers, q.info("s1"))
	}
}

// TestRenderUsageShowsTheKVBlockAsFleetLevel: the operator view carries
// the section, labelled fleet-level, shows a small peak to a tenth of a
// percent, and leaves out what was not reported.
func TestRenderUsageShowsTheKVBlockAsFleetLevel(t *testing.T) {
	rate, peak, q, h := 0.4, 0.005, 200.0, 80.0
	out := attach.RenderUsage(attach.UsageInfo{KVCache: &attach.KVCacheInfo{
		Fleet:   true,
		Session: &kvwindow.Window{Fleet: true, Queries: &q, Hits: &h, HitRate: &rate, PeakKVUsage: &peak, Windows: 2},
	}})
	for _, want := range []string{"fleet-level", "prefix hit rate 40.0%", "80 of 200 tokens hit", "peak KV 0.5%", "2 turn(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gauge") {
		t.Errorf("render shows an unreported gauge:\n%s", out)
	}
}
