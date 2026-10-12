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
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/mast/internal/injectedauth"
)

// With MAST_GOOGLE_AUTH=injected a Vertex model sends the placeholder
// bearer for the egress gateway to replace, and needs no ADC to build.
func TestInjectedGoogleAuthReachesTheRequest(t *testing.T) {
	clearVertexEnv(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "demo-project")
	t.Setenv("MAST_GOOGLE_AUTH", "injected")

	var mu sync.Mutex
	var authz string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authz = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	t.Cleanup(srv.Close)
	prevURL, prevResolve := vendorBaseURL, vendorResolve
	vendorBaseURL, vendorResolve = srv.URL, nil
	t.Cleanup(func() { vendorBaseURL, vendorResolve = prevURL, prevResolve })

	m, err := BuildModel(t.Context(), ProviderVertex, "gemini-3.5-flash", ModelOptions{})
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}, Config: &genai.GenerateContentConfig{}}
	for range m.GenerateContent(context.Background(), req, false) {
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "Bearer " + injectedauth.Placeholder; authz != want {
		t.Errorf("Authorization = %q, want %q", authz, want)
	}
}
