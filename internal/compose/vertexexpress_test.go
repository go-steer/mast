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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/workload"
)

// TestVertexExpressModeStillWorks: GOOGLE_GENAI_USE_VERTEXAI plus an API
// key and no project is Vertex AI express mode, which genai's env
// selection gave mast before core-models. The request that reaches the
// server has express mode's shape: the publishers/ path with no project
// prefix, the key in x-goog-api-key, and no bearer token.
func TestVertexExpressModeStillWorks(t *testing.T) {
	clearVertexEnv(t)
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "true")
	t.Setenv("GOOGLE_API_KEY", "express-key")

	var mu sync.Mutex
	var path, key, authz string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path, key, authz = r.URL.Path, r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
	}))
	t.Cleanup(srv.Close)
	prev := vendorBaseURL
	vendorBaseURL = srv.URL
	t.Cleanup(func() { vendorBaseURL = prev })

	m, err := BuildModel(t.Context(), "", "gemini-3.5-flash", ModelOptions{})
	if err != nil {
		t.Fatalf("BuildModel in express mode: %v", err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}, Config: &genai.GenerateContentConfig{}}
	for _, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if path != "/v1beta1/publishers/google/models/gemini-3.5-flash:generateContent" {
		t.Errorf("path = %q, want express mode's publishers/ path with no project prefix", path)
	}
	if key != "express-key" || authz != "" {
		t.Errorf("x-goog-api-key = %q, Authorization = %q; want the key and no bearer token", key, authz)
	}
}

// TestVertexExpressNeedsNoProject: a project or location in the
// environment wins over an implicit key, as in genai, and the `vertex`
// alias is ADC by design; neither is express mode.
func TestVertexExpressNeedsNoProject(t *testing.T) {
	clearVertexEnv(t)
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "true")
	t.Setenv("GOOGLE_API_KEY", "k")
	if _, ok := vertexExpressKey(""); !ok {
		t.Fatal("no project, a key: want express mode")
	}
	if _, ok := vertexExpressKey(ProviderVertex); ok {
		t.Error("the vertex alias took express mode; it is ADC by design")
	}
	t.Setenv("GOOGLE_CLOUD_PROJECT", "p")
	if _, ok := vertexExpressKey(""); ok {
		t.Error("a project in the environment still took express mode")
	}
}

// TestAdapterNoticesReachTheLog: a notice the Gemini adapter raises —
// here, a pre-3.0 model running without its built-ins because the
// request also carries function tools — reaches mast's log rather than
// being discarded.
func TestAdapterNoticesReachTheLog(t *testing.T) {
	offlineGeminiCreds(t)
	recordWire(t)
	var mu sync.Mutex
	var lines []string
	prev := vendorLogf
	vendorLogf = func(family string) func(string, ...any) {
		return func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, family+": "+format)
		}
	}
	t.Cleanup(func() { vendorLogf = prev })

	on := true
	m, err := BuildModel(t.Context(), "", "gemini-2.5-flash", ModelOptions{BuiltinTools: workload.BuiltinTools{WebSearch: &on}})
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{
			{Name: "finish_task", Description: "done"},
		}}}},
	}
	for range m.GenerateContent(context.Background(), req, false) {
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "gemini: ") {
		t.Fatalf("logged %q, want the adapter's pre-3.0 built-in notice under the gemini family", lines)
	}
}
