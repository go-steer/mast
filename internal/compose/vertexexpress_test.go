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
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/workload"
)

// TestVertexExpressModeStillWorks: GOOGLE_GENAI_USE_VERTEXAI plus an API
// key and no project is Vertex AI express mode, which genai's env
// selection gave mast before core-models, and which core-models'
// vertex-express profile now serves. The request that reaches the server
// has express mode's shape: the publishers/ path with no project prefix,
// the key in x-goog-api-key, and no bearer token. Both keys genai
// accepted work, GOOGLE_API_KEY first; and genai logs no "will take
// precedence" warning, which a placeholder project used to provoke.
func TestVertexExpressModeStillWorks(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"GOOGLE_API_KEY":     {"GOOGLE_API_KEY": "express-key"},
		"GEMINI_API_KEY":     {"GEMINI_API_KEY": "express-key"},
		"both, GOOGLE first": {"GOOGLE_API_KEY": "express-key", "GEMINI_API_KEY": "other-key"},
	} {
		t.Run(name, func(t *testing.T) { expressRequest(t, env) })
	}
}

func expressRequest(t *testing.T, env map[string]string) {
	clearVertexEnv(t)
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "true")
	for k, v := range env {
		t.Setenv(k, v)
	}
	var logged bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prevOut) })

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
	if strings.Contains(logged.String(), "take precedence") {
		t.Errorf("genai logged a precedence warning in express mode: %q", logged.String())
	}
}

// TestVertexExpressNeedsNoProject: a project or location in the
// environment wins over an implicit key, as in genai, and the `vertex`
// alias is ADC by design; neither is express mode.
func TestVertexExpressNeedsNoProject(t *testing.T) {
	clearVertexEnv(t)
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "true")
	t.Setenv("GOOGLE_API_KEY", "k")
	if !vertexExpress("") {
		t.Fatal("no project, a key: want express mode")
	}
	if vertexExpress(ProviderVertex) {
		t.Error("the vertex alias took express mode; it is ADC by design")
	}
	t.Setenv("GOOGLE_CLOUD_PROJECT", "p")
	if vertexExpress("") {
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

// TestVertexExpressFollowsGenaiLocationRules pins the location half of
// genai v1.72's precedence: GOOGLE_CLOUD_REGION counts as a location —
// so it disables express mode, as in genai — unless GOOGLE_CLOUD_LOCATION
// is set, even to the empty string, which hides it.
func TestVertexExpressFollowsGenaiLocationRules(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want bool
	}{
		"region alone disables":           {map[string]string{"GOOGLE_CLOUD_REGION": "us-east5"}, false},
		"location disables":               {map[string]string{"GOOGLE_CLOUD_LOCATION": "us-east5"}, false},
		"empty location hides the region": {map[string]string{"GOOGLE_CLOUD_LOCATION": "", "GOOGLE_CLOUD_REGION": "us-east5"}, true},
		"neither":                         {map[string]string{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			clearVertexEnv(t)
			t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "true")
			t.Setenv("GOOGLE_API_KEY", "k")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := vertexExpress(""); got != tc.want {
				t.Errorf("vertexExpress = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGeminiOnVertexFollowsGenaiBackendRules: GOOGLE_GENAI_USE_ENTERPRISE,
// when set, decides the backend whatever GOOGLE_GENAI_USE_VERTEXAI says,
// as in genai v1.72 — which is what mast's no-alias path did before it
// moved to core-models.
func TestGeminiOnVertexFollowsGenaiBackendRules(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want bool
	}{
		"vertexai true":                {map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true"}, true},
		"vertexai TRUE":                {map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "TRUE"}, true},
		"enterprise true":              {map[string]string{"GOOGLE_GENAI_USE_ENTERPRISE": "1"}, true},
		"enterprise false wins":        {map[string]string{"GOOGLE_GENAI_USE_ENTERPRISE": "false", "GOOGLE_GENAI_USE_VERTEXAI": "true"}, false},
		"enterprise empty wins":        {map[string]string{"GOOGLE_GENAI_USE_ENTERPRISE": "", "GOOGLE_GENAI_USE_VERTEXAI": "true"}, false},
		"nothing set is the Developer": {map[string]string{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			clearVertexEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := geminiOnVertex(""); got != tc.want {
				t.Errorf("geminiOnVertex = %v, want %v", got, tc.want)
			}
		})
	}
	if !geminiOnVertex(ProviderVertex) {
		t.Error("the vertex alias is Vertex whatever the environment says")
	}
}

// TestVendorLogfKeepsTheFamilyOutOfTheFormat: a profile name is operator
// text, so a % in it must reach the line as written.
func TestVendorLogfKeepsTheFamilyOutOfTheFormat(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = w
	vendorLogf("lab-100%")("cache %s evicted", "c1")
	os.Stderr = prev
	_ = w.Close()
	got, _ := io.ReadAll(r)
	if string(got) != "mast: lab-100%: cache c1 evicted\n" {
		t.Errorf("logged %q", got)
	}
}
