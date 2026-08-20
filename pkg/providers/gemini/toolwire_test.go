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

package gemini_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	adkgemini "google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"

	"github.com/go-steer/mast/internal/toolcatalog"
	geminiprov "github.com/go-steer/mast/pkg/providers/gemini"
)

// mixedToolsModel is a model id whose major version clears this
// package's Gemini 3.0+ gate for mixing built-in and function tools.
// A pre-3.0 id would make the built-ins silently drop out, and the
// request under test would no longer be the one production sends.
const mixedToolsModel = "gemini-3-pro-preview"

// The Gemini half of #168. Unlike the Anthropic one, mast writes no
// schema conversion here — ADK's model and genai's serializer own the
// path from a FunctionDeclaration to the wire, and this package only
// wraps the result. That makes this a dependency probe rather than a
// test of mast's own code, and it is worth having for exactly that
// reason: the whole of #154 was mast trusting that a declaration
// reaches the model, on the one path where it did not. The same trust
// is unexamined here.
//
// What it would catch: genai dropping ParametersJsonSchema on
// serialization (the field is a pure pass-through `any` with no
// validation, so nothing would complain), or this package's built-in
// tool injection displacing the function declarations it is supposed
// to sit alongside.
//
// It runs offline. The client is pointed at an httptest server via
// HTTPOptions.BaseURL, so no credentials and no network.
func TestToolWire_EveryDispatchedToolReachesGemini(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	endpoint, stop := toolcatalog.StartStubMCP()
	t.Cleanup(stop)

	catalog, err := toolcatalog.Build(ctx, toolcatalog.Config{MCPEndpoint: endpoint})
	if err != nil {
		t.Fatalf("build tool catalog: %v", err)
	}
	if len(catalog) < 8 {
		t.Fatalf("catalog has only %d tools — too few to be measuring anything:\n%s", len(catalog), toolcatalog.Summary(catalog))
	}

	decls := make([]*genai.FunctionDeclaration, 0, len(catalog))
	for _, e := range catalog {
		decls = append(decls, e.Declaration)
	}

	llm, captured := offlineGemini(t, mixedToolsModel)
	for _, err := range llm.GenerateContent(ctx, &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("enumerate the tools", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{FunctionDeclarations: decls}},
		},
	}, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}

	wire := geminiWire(t, captured)
	for _, problem := range toolcatalog.Verify(catalog, wire) {
		t.Error(problem)
	}
	if t.Failed() {
		t.Logf("catalog under test:\n%s", toolcatalog.Summary(catalog))
	}
}

// offlineGemini builds the production Gemini stack — ADK's model under
// this package's Wrap, the same two layers internal/compose assembles —
// pointed at a fake generateContent endpoint. Built-ins are on, as
// compose turns them on, so the test also covers them coexisting with
// function declarations rather than replacing them.
func offlineGemini(t *testing.T, modelID string) (adkmodel.LLM, *capturedBody) {
	t.Helper()

	captured := &capturedBody{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"catalogued"}]},"finishReason":"STOP"}]}`)
	}))
	t.Cleanup(srv.Close)

	base, err := adkgemini.NewModel(context.Background(), modelID, &genai.ClientConfig{
		Backend:     genai.BackendGeminiAPI,
		APIKey:      "test-key-not-real",
		HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL},
	})
	if err != nil {
		t.Fatalf("adkgemini.NewModel: %v", err)
	}
	return geminiprov.Wrap(base, geminiprov.Options{
		BuiltinTools:                     geminiprov.DefaultBuiltinTools(),
		IncludeServerSideToolInvocations: true,
	}), captured
}

type capturedBody struct {
	body map[string]any
}

// geminiWire reads the function declarations back out of the captured
// generateContent body. Gemini nests them under tools[].
// functionDeclarations[], and a declaration's arguments arrive under
// either "parameters" (the typed spelling) or "parametersJsonSchema" —
// which one is not this test's business, so it accepts whichever is
// present and fails only when neither is.
func geminiWire(t *testing.T, captured *capturedBody) toolcatalog.Wire {
	t.Helper()

	raw, ok := captured.body["tools"]
	if !ok {
		t.Fatalf("request body carries no tools field at all: %v", captured.body)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("tools is %T, want a JSON array", raw)
	}

	wire := toolcatalog.Wire{}
	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("tools[%d] is %T, want a JSON object", i, item)
		}
		fns, ok := entry["functionDeclarations"].([]any)
		if !ok {
			// A built-in tool entry (googleSearch, urlContext); it
			// carries no declarations and is not what this measures.
			continue
		}
		for j, f := range fns {
			fn, ok := f.(map[string]any)
			if !ok {
				t.Fatalf("tools[%d].functionDeclarations[%d] is %T, want a JSON object", i, j, f)
			}
			name, _ := fn["name"].(string)
			if name == "" {
				t.Fatalf("tools[%d].functionDeclarations[%d] has no name: %v", i, j, fn)
			}
			schema, ok := fn["parameters"].(map[string]any)
			if !ok {
				schema, ok = fn["parametersJsonSchema"].(map[string]any)
			}
			if !ok {
				t.Errorf("%s: neither parameters nor parametersJsonSchema is a JSON object on the wire — Gemini is shown a tool with no arguments (got %v)", name, fn)
				continue
			}
			wire[name] = schema
		}
	}
	return wire
}

// TestToolWire_GeminiBuiltinsDoNotDisplaceFunctionTools states the
// coexistence separately from the schema invariant, because the two
// fail for different reasons and a combined failure would not say
// which. Built-ins are appended as their own *genai.Tool entries; a
// wrapper that replaced Config.Tools instead of appending would take
// every function tool off the wire.
func TestToolWire_GeminiBuiltinsDoNotDisplaceFunctionTools(t *testing.T) {
	t.Parallel()

	llm, captured := offlineGemini(t, mixedToolsModel)
	decl := &genai.FunctionDeclaration{
		Name:        "lone_tool",
		Description: "the only function tool in the request",
		ParametersJsonSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"arg": map[string]any{"type": "string"}},
			"required":   []any{"arg"},
		},
	}
	for _, err := range llm.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{decl}}},
		},
	}, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}

	wire := geminiWire(t, captured)
	if _, ok := wire["lone_tool"]; !ok {
		t.Fatalf("the function tool is absent from a request that also carries built-ins: %v", captured.body["tools"])
	}
	if len(captured.body["tools"].([]any)) < 2 {
		t.Errorf("built-ins did not reach the wire alongside the function tool: %v", captured.body["tools"])
	}
}
