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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/profile"
)

// vllmMetrics serves a vLLM-shaped Prometheus exposition whose prefix
// cache counters climb by 100 queried / 40 hit tokens per scrape.
func vllmMetrics(t *testing.T) *httptest.Server {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := n.Add(1)
		fmt.Fprintf(w, "# TYPE vllm:prefix_cache_queries_total counter\nvllm:prefix_cache_queries_total{engine=\"0\",model_name=\"m\"} %d\n", 1000+100*i)
		fmt.Fprintf(w, "# TYPE vllm:prefix_cache_hits_total counter\nvllm:prefix_cache_hits_total{engine=\"0\",model_name=\"m\"} %d\n", 400+40*i)
		fmt.Fprintf(w, "# TYPE vllm:kv_cache_usage_perc gauge\nvllm:kv_cache_usage_perc{engine=\"0\",model_name=\"m\"} 0.25\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestKVSamplerOnlyForProfilesThatPublishMetrics: a sampler exists for a
// profile with a metrics endpoint and a server kind, and for nothing
// else — a mast alias, a profile without metrics_url, or Ollama's
// metrics: none all get nil, which callers read as "skip".
func TestKVSamplerOnlyForProfilesThatPublishMetrics(t *testing.T) {
	srv := vllmMetrics(t)
	RegisterProfiles([]profile.Profile{
		{Name: "lab", Extends: "vllm", BaseURL: srv.URL + "/v1", MetricsURL: srv.URL + "/metrics"},
		{Name: "lab-nometrics", Extends: "vllm", BaseURL: srv.URL + "/v1"},
		{Name: "lab-ollama", Extends: "ollama", MetricsURL: srv.URL + "/metrics"},
	})
	t.Cleanup(func() { RegisterProfiles(nil) })
	ctx := context.Background()

	s, err := KVSampler(ctx, "lab", "m")
	if err != nil || s == nil {
		t.Fatalf("KVSampler(lab) = %v, %v; want a sampler", s, err)
	}
	r, err := s.Snapshot(ctx)
	if err != nil || r.Queries == nil {
		t.Fatalf("Snapshot = %+v, %v", r, err)
	}
	for _, prov := range []string{"vertex", "anthropic", "lab-nometrics", "lab-ollama"} {
		if s, err := KVSampler(ctx, prov, "m"); s != nil || err != nil {
			t.Errorf("KVSampler(%s) = %v, %v; want nil, nil", prov, s, err)
		}
	}
}

// TestKVSamplerResolvesLikeTheModel: the sampler's profile is resolved
// with the same options as the model's (resolveOptions), so under
// MAST_GOOGLE_AUTH=injected a google_adc profile resolves without
// Application Default Credentials, as its model does. The http metrics
// endpoint gets no Google token (same-origin rule), and the scrape works.
func TestKVSamplerResolvesLikeTheModel(t *testing.T) {
	srv := vllmMetrics(t)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent/adc.json")
	RegisterProfiles([]profile.Profile{{
		Name: "gw", Extends: "vllm", BaseURL: srv.URL + "/v1", MetricsURL: srv.URL + "/metrics",
		Auth: auth.Config{Kind: auth.GoogleADC},
	}})
	t.Cleanup(func() { RegisterProfiles(nil) })
	ctx := context.Background()

	t.Setenv("MAST_GOOGLE_AUTH", "")
	if _, err := KVSampler(ctx, "gw", "m"); err == nil {
		t.Fatal("a google_adc profile resolved with no ADC and no injected auth; the test proves nothing")
	}
	t.Setenv("MAST_GOOGLE_AUTH", "injected")
	s, err := KVSampler(ctx, "gw", "m")
	if err != nil || s == nil {
		t.Fatalf("KVSampler under injected auth = %v, %v; want a sampler", s, err)
	}
	if _, err := s.Snapshot(ctx); err != nil {
		t.Errorf("Snapshot: %v", err)
	}
}
