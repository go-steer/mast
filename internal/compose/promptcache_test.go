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
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/profile"

	mastusage "github.com/go-steer/mast/internal/providers/usage"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/workload"
)

// sentCacheControls returns every cache_control marker m puts on the
// wire for a request with a system prompt and one user turn. The
// recorder answers in Gemini's shape, so the Claude call itself fails;
// the request body it sent is all this reads.
func sentCacheControls(t *testing.T, rec *wireRecorder, m adkmodel.LLM) []map[string]any {
	t.Helper()
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("why is the pod pending?", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("You triage Kubernetes incidents.", genai.RoleUser),
		},
	}
	for range m.GenerateContent(context.Background(), req, false) {
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.body == nil {
		t.Fatal("the recorder saw no request")
	}
	var out []map[string]any
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if cc, ok := x["cache_control"].(map[string]any); ok {
				out = append(out, cc)
			}
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(rec.body)
	return out
}

// TestClaudePromptCacheFollowsTheBundle reads the bundle's prompt_cache
// block off the request a claude-* model actually sends: no breakpoints
// unless the bundle turns caching on (the previous release sent none),
// Anthropic's default (5-minute) TTL when it does, and the 1-hour TTL on
// every breakpoint when it asks for one. A ttl without enabled: true
// turns nothing on.
func TestClaudePromptCacheFollowsTheBundle(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "offline-not-a-real-key")
	off, on := false, true
	for name, tc := range map[string]struct {
		pc      workload.PromptCache
		markers bool
		ttl     string // "" = no ttl field, Anthropic's 5-minute default
	}{
		"no bundle":   {},
		"enabled":     {pc: workload.PromptCache{Enabled: &on}, markers: true},
		"5m":          {pc: workload.PromptCache{Enabled: &on, TTL: workload.PromptCacheTTL5m}, markers: true},
		"disabled":    {pc: workload.PromptCache{Enabled: &off}},
		"ttl alone":   {pc: workload.PromptCache{TTL: workload.PromptCacheTTL1h}},
		"one hour":    {pc: workload.PromptCache{Enabled: &on, TTL: workload.PromptCacheTTL1h}, markers: true, ttl: "1h"},
		"off wins 1h": {pc: workload.PromptCache{Enabled: &off, TTL: workload.PromptCacheTTL1h}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := recordWire(t)
			m, err := BuildModel(t.Context(), "anthropic", "claude-haiku-4-5", ModelOptions{PromptCache: tc.pc})
			if err != nil {
				t.Fatalf("BuildModel: %v", err)
			}
			got := sentCacheControls(t, rec, m)
			if !tc.markers {
				if len(got) != 0 {
					t.Fatalf("caching is off but the request carries %d cache_control markers: %v", len(got), got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatal("caching is on but the request carries no cache_control marker")
			}
			for _, cc := range got {
				ttl, _ := cc["ttl"].(string)
				if tc.ttl == "" && ttl != "" && ttl != "5m" {
					t.Errorf("marker %v: want the 5-minute TTL", cc)
				}
				if tc.ttl != "" && ttl != tc.ttl {
					t.Errorf("marker %v: want ttl %q", cc, tc.ttl)
				}
			}
		})
	}
}

// TestAOneHourCacheWriteIsBilledAtTheOneHourRate follows a 1-hour write
// from the wire to the dollar figure: the provider reports the share
// under usage.cache_creation, core-models' adapter records it, the
// bridge carries it onto mast's sidecar, and the meter bills it at the
// catalog's 1-hour rate rather than the 5-minute one.
//
// The counts are the measured warm turn pkg/budget's rate-card test
// pins (28,804 tokens written, 10 fresh, 4 out), written at 1h.
func TestAOneHourCacheWriteIsBilledAtTheOneHourRate(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "offline-not-a-real-key")
	// The adapter always uses the Messages streaming API, so the
	// recorder answers in server-sent events. The usage — and with it
	// the 1-hour split — rides on message_start.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []struct{ name, data string }{
			{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1,"cache_creation_input_tokens":28804,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":28804}}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			_, _ = io.WriteString(w, "event: "+ev.name+"\ndata: "+ev.data+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	prev := vendorBaseURL
	vendorBaseURL = srv.URL
	t.Cleanup(func() { vendorBaseURL = prev })

	on := true
	m, err := BuildModel(t.Context(), "anthropic", "claude-sonnet-5",
		ModelOptions{PromptCache: workload.PromptCache{Enabled: &on, TTL: workload.PromptCacheTTL1h}})
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	var final *adkmodel.LLMResponse
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("why is the pod pending?", genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{},
	}
	for r, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		final = r
	}

	d, ok := final.CustomMetadata[budget.DetailKey].(*mastusage.Detail)
	if !ok || d.CacheWriteTokens == nil || *d.CacheWriteTokens != 28804 ||
		d.CacheWrite1hTokens == nil || *d.CacheWrite1hTokens != 28804 {
		v := func(p *int64) any {
			if p == nil {
				return nil
			}
			return *p
		}
		t.Fatalf("sidecar writes=%v 1h=%v read=%v, want 28804 writes, all of them 1h", v(d.CacheWriteTokens), v(d.CacheWrite1hTokens), v(d.CacheReadTokens))
	}

	meter := budget.NewMeter(ModelLimits("anthropic", "claude-sonnet-5"))
	ev := session.NewEvent(context.Background(), "inv-1")
	ev.LLMResponse = *final
	if err := meter.Observe(ev); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	_, got, _ := meter.Snapshot()
	// anthropic/claude-sonnet-5: $2 input, $4 1h write, $10 output per MTok.
	want := (10*2.0 + 28804*4.0 + 4*10.0) / 1e6
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("metered $%.6f, want $%.6f (at the 5-minute rate it would be $%.6f)", got, want, (10*2.0+28804*2.5+4*10.0)/1e6)
	}
}

// TestAnthropicProfileModelsFollowThePromptCache: a provider profile on
// the anthropic dialect gets the bundle's prompt cache too — off unless
// the bundle turns it on, where the library's own default would be on.
// Off then on in one process, so a profile cache keyed on the name
// alone would hand the second build the first one's posture.
func TestAnthropicProfileModelsFollowThePromptCache(t *testing.T) {
	t.Setenv("LAB_ANTHROPIC_KEY", "offline-not-a-real-key")
	rec := &wireRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		rec.body = body
		rec.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	open := true
	RegisterProfiles([]profile.Profile{{
		Name:       "lab-claude",
		Extends:    "anthropic",
		BaseURL:    srv.URL,
		Auth:       auth.Config{Kind: auth.APIKey, Env: "LAB_ANTHROPIC_KEY"},
		OpenModels: &open,
	}})
	t.Cleanup(func() { RegisterProfiles(nil) })

	on := true
	for _, tc := range []struct {
		name    string
		pc      workload.PromptCache
		markers bool
	}{
		{"off by default", workload.PromptCache{}, false},
		{"on when the bundle asks", workload.PromptCache{Enabled: &on}, true},
	} {
		m, err := BuildModel(t.Context(), "lab-claude", "house-sonnet", ModelOptions{PromptCache: tc.pc})
		if err != nil {
			t.Fatalf("%s: BuildModel: %v", tc.name, err)
		}
		if got := sentCacheControls(t, rec, m); (len(got) > 0) != tc.markers {
			t.Errorf("%s: %d cache_control markers, want markers=%v", tc.name, len(got), tc.markers)
		}
	}
}
