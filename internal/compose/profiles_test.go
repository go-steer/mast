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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/core-models/profile"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/internal/toolcatalog"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/workload"
)

// chatServer is a fake chat-completions endpoint that records request
// bodies and answers every call with reply.
type chatServer struct {
	mu     sync.Mutex
	bodies []map[string]any
	srv    *httptest.Server
}

const chatReply = `{"id":"chatcmpl-1","model":"/models/qwen3-coder","choices":[{"index":0,"message":{"role":"assistant","content":"pods look fine"},"finish_reason":"stop"}],
"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":64}}}`

func newChatServer(t *testing.T) *chatServer {
	t.Helper()
	s := &chatServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		s.mu.Lock()
		s.bodies = append(s.bodies, b)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatReply)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// withLabProfile registers a self-hosted profile pointed at s and
// unregisters it when the test ends. Not parallel-safe: the registry is
// process-wide, as the --provider alias it extends is.
func withLabProfile(t *testing.T, s *chatServer) {
	t.Helper()
	RegisterProfiles([]profile.Profile{{
		Name:       "lab",
		Extends:    "vllm",
		BaseURL:    s.srv.URL + "/v1",
		OpenModels: new(false),
		Models:     []profile.Model{{ID: "Qwen/Qwen3-Coder-Next", Tier: profile.Mid}, {ID: "qwen3:1.7b", Tier: profile.Small}},
		Tiers:      map[profile.Tier]string{profile.Mid: "Qwen/Qwen3-Coder-Next", profile.Small: "qwen3:1.7b"},
	}})
	t.Cleanup(func() { RegisterProfiles(nil) })
}

func generate(t *testing.T, m adkmodel.LLM, req *adkmodel.LLMRequest) *adkmodel.LLMResponse {
	t.Helper()
	var final *adkmodel.LLMResponse
	for r, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		final = r
	}
	return final
}

func TestAProfileModelRunsThroughADKAndTheMeter(t *testing.T) {
	s := newChatServer(t)
	withLabProfile(t, s)

	m, err := NewRuntimeModel(context.Background(), "lab", "Qwen/Qwen3-Coder-Next", workload.BuiltinTools{})
	if err != nil {
		t.Fatal(err)
	}
	resp := generate(t, m, &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("how are the pods?", genai.RoleUser)}})
	if resp.Content.Parts[0].Text != "pods look fine" || resp.ModelVersion != "Qwen/Qwen3-Coder-Next" {
		t.Fatalf("response = %+v", resp)
	}

	// The bridge put mast's sidecar where pkg/budget looks for it.
	d, ok := resp.CustomMetadata[budget.DetailKey].(budget.Detailer)
	if !ok {
		t.Fatalf("CustomMetadata[%q] = %T, want a budget.Detailer", budget.DetailKey, resp.CustomMetadata[budget.DetailKey])
	}
	if b := d.UsageBuckets(); b.CacheReadTokens == nil || *b.CacheReadTokens != 64 || b.CacheWriteTokens != nil {
		t.Errorf("buckets = %+v, want 64 cached and writes not reported", b)
	}

	// The meter counts the tokens, prices nothing, and says so.
	meter := budget.NewMeter(ModelLimits("lab", "Qwen/Qwen3-Coder-Next"))
	ev := session.NewEvent(context.Background(), "inv-1")
	ev.LLMResponse = *resp
	if err := meter.Observe(ev); err != nil {
		t.Fatal(err)
	}
	tokens, cost, calls := meter.Snapshot()
	if tokens != 150 || cost != 0 || calls != 1 || meter.Unpriced() != 1 {
		t.Errorf("meter: tokens %d cost %v calls %d unpriced %d; want 150, $0, 1, 1", tokens, cost, calls, meter.Unpriced())
	}
}

func TestProfileBackendTiersAndRates(t *testing.T) {
	withLabProfile(t, newChatServer(t))
	if got := Backend("lab", "Qwen/Qwen3-Coder-Next"); got != "lab" {
		t.Errorf("Backend = %q, want the profile's backend", got)
	}
	if got := Backend("vertex-maas", "openai/gpt-oss-20b-maas"); got != "vertex-maas" {
		t.Errorf("Backend(vertex-maas) = %q", got)
	}
	if got := RatePer1K("lab", "Qwen/Qwen3-Coder-Next"); got != 0 {
		t.Errorf("RatePer1K = %v, want 0: no invented flat rate for a profile", got)
	}
	if got, err := TierModelName("lab", "", "small"); err != nil || got != "qwen3:1.7b" {
		t.Errorf("TierModelName(small) = %q, %v", got, err)
	}
	if _, err := TierModelName("lab", "", "frontier"); err == nil || !strings.Contains(err.Error(), `declares no "frontier" tier`) {
		t.Errorf("TierModelName(frontier) = %v", err)
	}
	// mast's own aliases are untouched by the registry.
	if IsProfileProvider("gemini") || IsProfileProvider("") || !IsProfileProvider("ollama") || IsProfileProvider("nope") {
		t.Error("IsProfileProvider misclassifies")
	}
}

func TestProfileModelRefusals(t *testing.T) {
	withLabProfile(t, newChatServer(t))
	ctx := context.Background()
	if _, err := BuildModel(ctx, "lab", "llama3", workload.BuiltinTools{}); err == nil || !strings.Contains(err.Error(), `does not serve model "llama3"`) {
		t.Errorf("unserved model = %v", err)
	}
	on := true
	if _, err := BuildModel(ctx, "lab", "Qwen/Qwen3-Coder-Next", workload.BuiltinTools{WebSearch: &on}); err == nil || !strings.Contains(err.Error(), "web_search") {
		t.Errorf("built-in tools on a profile = %v, want a refusal naming them", err)
	}
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	if _, err := BuildModel(ctx, "vertex-maas", "openai/gpt-oss-20b-maas", workload.BuiltinTools{}); err == nil || !strings.Contains(err.Error(), "GOOGLE_CLOUD_PROJECT") {
		t.Errorf("vertex-maas with no project = %v, want a construction-time refusal", err)
	}
}

func TestCeilingOnAnUnpricedProfileModelIsRefused(t *testing.T) {
	withLabProfile(t, newChatServer(t))
	err := CheckCeilingPriced("workload triage", "lab", "Qwen/Qwen3-Coder-Next", 5)
	if err == nil || !strings.Contains(err.Error(), "cannot be enforced") || !strings.Contains(err.Error(), "workload triage") {
		t.Errorf("CheckCeilingPriced = %v", err)
	}
	for _, tc := range []struct{ provider, model string }{
		{"lab", "Qwen/Qwen3-Coder-Next"}, // no ceiling: below
		{"gemini", "gemini-3.7-flash"},
		{"", "echo"},
	} {
		limit := 5.0
		if tc.provider == "lab" {
			limit = 0
		}
		if err := CheckCeilingPriced("w", tc.provider, tc.model, limit); err != nil {
			t.Errorf("CheckCeilingPriced(%s, %s, %v) = %v", tc.provider, tc.model, limit, err)
		}
	}
}

// The downstream half of core-models' toolwire contract: mast's real,
// captured tool catalog through the openai-chat adapter, held to the
// same Verify every adapter is.
func TestToolWire_ProfileModelPresentsEveryTool(t *testing.T) {
	s := newChatServer(t)
	withLabProfile(t, s)

	endpoint, stop := toolcatalog.StartStubMCP()
	t.Cleanup(stop)
	catalog, err := toolcatalog.Build(context.Background(), toolcatalog.Config{MCPEndpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) < 8 {
		t.Fatalf("catalog has only %d tools — too few to be measuring anything:\n%s", len(catalog), toolcatalog.Summary(catalog))
	}
	decls := make([]*genai.FunctionDeclaration, 0, len(catalog))
	for _, e := range catalog {
		decls = append(decls, e.Declaration)
	}
	m, err := BuildModel(context.Background(), "lab", "Qwen/Qwen3-Coder-Next", workload.BuiltinTools{})
	if err != nil {
		t.Fatal(err)
	}
	generate(t, m, &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("enumerate the tools", genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: decls}}},
	})

	wire := toolcatalog.Wire{}
	s.mu.Lock()
	tools, _ := s.bodies[0]["tools"].([]any)
	s.mu.Unlock()
	for _, raw := range tools {
		fn := raw.(map[string]any)["function"].(map[string]any)
		schema, _ := fn["parameters"].(map[string]any)
		wire[fn["name"].(string)] = schema
	}
	if len(wire) != len(catalog) {
		t.Errorf("sent %d tools, %d arrived on the wire", len(catalog), len(wire))
	}
	for _, problem := range toolcatalog.Verify(catalog, wire) {
		t.Error(problem)
	}
	if t.Failed() {
		t.Logf("catalog under test:\n%s", toolcatalog.Summary(catalog))
	}
}

// A specialist's claude-* override under a profile root reaches
// Anthropic, picking its backend from the environment the way it does
// under a gemini root.
func TestAClaudeOverrideUnderAProfileRoot(t *testing.T) {
	withLabProfile(t, newChatServer(t))
	t.Setenv("ANTHROPIC_API_KEY", "offline-not-a-real-key")
	if _, err := BuildModel(context.Background(), "lab", "claude-haiku-4-5", workload.BuiltinTools{}); err != nil {
		t.Fatalf("BuildModel(claude under a profile root) = %v", err)
	}
	if got := Backend("lab", "claude-haiku-4-5"); got != "anthropic" {
		t.Errorf("Backend = %q, want anthropic", got)
	}
	if got := Backend("lab", "gemini-3.7-flash"); got != ProviderGemini && got != ProviderVertex {
		t.Errorf("Backend(gemini under a profile root) = %q", got)
	}
}

// Vertex AI partner models are priced from the catalog
// (vertex-maas/<publisher>/<model> rows), so a cost ceiling on one is
// enforceable; a self-hosted model is still unpriced, and refused.
func TestVertexMaaSModelsArePriced(t *testing.T) {
	withLabProfile(t, newChatServer(t))
	if err := CheckCeilingPriced("workload triage", "vertex-maas", "zai-org/glm-5.2-maas", 5); err != nil {
		t.Errorf("CheckCeilingPriced(glm-5.2 on vertex-maas) = %v, want priced", err)
	}
	if r := RatePer1K("vertex-maas", "zai-org/glm-5.2-maas"); r <= 0 {
		t.Errorf("RatePer1K = %v, want the catalog's blended rate", r)
	}
	if err := CheckCeilingPriced("workload triage", "lab", "Qwen/Qwen3-Coder-Next", 5); err == nil {
		t.Error("a self-hosted model became priced")
	}
}
