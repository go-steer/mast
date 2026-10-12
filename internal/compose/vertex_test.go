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
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/profile"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// clearVertexEnv unsets every variable the two Gemini backends read, so
// a case declares its whole environment rather than inheriting the
// developer's. t.Setenv also fails the test if it ever runs in
// parallel, which is what we want around process-global state.
//
// Unset, not blanked: genai's selection treats a variable set to the
// empty string differently from one that is absent (an empty
// GOOGLE_GENAI_USE_ENTERPRISE selects the Developer API; an empty
// GOOGLE_CLOUD_LOCATION hides GOOGLE_CLOUD_REGION), and mast mirrors it.
// t.Setenv first registers the restore.
func clearVertexEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GOOGLE_GENAI_USE_VERTEXAI",
		"GOOGLE_GENAI_USE_ENTERPRISE",
		"GOOGLE_CLOUD_PROJECT",
		"GOOGLE_CLOUD_LOCATION",
		"GOOGLE_CLOUD_REGION",
		"GOOGLE_API_KEY",
		"GEMINI_API_KEY",
	} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRequireVertexProject: Gemini on Vertex AI needs a project, and
// the refusal names GOOGLE_CLOUD_PROJECT whichever way Vertex was
// selected — and, when an API key was set but a location in the
// environment ruled out express mode, which variable did it.
func TestRequireVertexProject(t *testing.T) {
	for name, tc := range map[string]struct {
		provider string
		env      map[string]string
		want     []string // substrings; nil = no error
	}{
		"project set":                {ProviderVertex, map[string]string{"GOOGLE_CLOUD_PROJECT": "p"}, nil},
		"vertex alias, no project":   {ProviderVertex, nil, []string{"--provider=vertex", "GOOGLE_CLOUD_PROJECT"}},
		"environment, no project":    {"", map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true"}, []string{"the environment", "GOOGLE_CLOUD_PROJECT"}},
		"key, but a region":          {"", map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true", "GOOGLE_API_KEY": "k", "GOOGLE_CLOUD_REGION": "us-east5"}, []string{"GOOGLE_CLOUD_REGION is too", "express mode"}},
		"key, but a location":        {"", map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true", "GEMINI_API_KEY": "k", "GOOGLE_CLOUD_LOCATION": "us-east5"}, []string{"GOOGLE_CLOUD_LOCATION is too"}},
		"alias ignores express hint": {ProviderVertex, map[string]string{"GOOGLE_API_KEY": "k", "GOOGLE_CLOUD_REGION": "us-east5"}, []string{"--provider=vertex"}},
	} {
		t.Run(name, func(t *testing.T) {
			clearVertexEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			err := requireVertexProject(tc.provider)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no error, want a refusal")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %q, want it to mention %q", err, w)
				}
			}
			if tc.provider == ProviderVertex && strings.Contains(err.Error(), "express mode") {
				t.Errorf("the vertex alias is ADC by design, but the error offers express mode: %q", err)
			}
		})
	}
}

// TestGeminiOnVertex covers the empty-chunk tolerance switch, which has
// to follow the backend actually in use. Before the alias it could only
// read the environment; a run on Vertex by alias would have had the
// tolerance off and seen Vertex's candidate-less heartbeat chunks as
// malformed.
func TestGeminiOnVertex(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		env      string
		want     bool
	}{
		{"alias, no env", ProviderVertex, "", true},
		{"alias and env disagree in the harmless direction", ProviderVertex, "false", true},
		{"no alias, env true", "", "true", true},
		{"no alias, env 1", "", "1", true},
		{"no alias, env TRUE", "", "TRUE", true},
		{"gemini alias, env true", ProviderGemini, "true", true},
		{"gemini alias, no env", ProviderGemini, "", false},
		{"no alias, no env", "", "", false},
		{"no alias, env false", "", "false", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearVertexEnv(t)
			t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", tt.env)

			if got := geminiOnVertex(tt.provider); got != tt.want {
				t.Errorf("geminiOnVertex(%q) with GOOGLE_GENAI_USE_VERTEXAI=%q = %v, want %v", tt.provider, tt.env, got, tt.want)
			}
		})
	}
}

// TestBuildModel_VertexAliasRefusesWithoutProject checks that the
// refusal reaches BuildModel's caller — construction time, at startup,
// not the first turn of an incident.
func TestBuildModel_VertexAliasRefusesWithoutProject(t *testing.T) {
	clearVertexEnv(t)

	if _, err := BuildModel(t.Context(), ProviderVertex, "gemini-3.5-flash", ModelOptions{}); err == nil ||
		!strings.Contains(err.Error(), "GOOGLE_CLOUD_PROJECT") {
		t.Errorf("BuildModel(vertex, gemini-3.5-flash) with no project: err = %v, want the project guidance", err)
	}
}

// TestTierModelName_VertexResolves closes the loop the tier table had
// left open: internal/taskclass has carried a "vertex" family since the
// port, but nothing could ask for it. A tier under the alias must
// resolve to the same Gemini ids the gemini alias gets — a backend is
// not a different model line.
func TestTierModelName_VertexResolves(t *testing.T) {
	for _, tier := range []string{"small", "mid", "frontier"} {
		viaVertex, err := TierModelName(ProviderVertex, "", tier)
		if err != nil {
			t.Fatalf("TierModelName(vertex, %q) = err %v", tier, err)
		}
		viaGemini, err := TierModelName(ProviderGemini, "", tier)
		if err != nil {
			t.Fatalf("TierModelName(gemini, %q) = err %v", tier, err)
		}
		if viaVertex != viaGemini {
			t.Errorf("tier %q: vertex resolved %q, gemini resolved %q — want the same id", tier, viaVertex, viaGemini)
		}
		if !strings.HasPrefix(viaVertex, "gemini-") {
			t.Errorf("tier %q under vertex resolved %q, want a gemini-* id", tier, viaVertex)
		}
	}
}

// fakeToken is a Google token source for tests that open an ADC-backed
// built-in profile without real credentials.
type fakeToken struct{}

func (fakeToken) Token(context.Context) (string, error) { return "test-token", nil }

// TestVertexLocationReachesTheRequest: since core-models v0.6.2 the
// built-in vertex profile resolves mast's whole location chain itself,
// GOOGLE_CLOUD_REGION included, so mast no longer pins it. This reads the
// location off the request path, end to end through buildGemini.
func TestVertexLocationReachesTheRequest(t *testing.T) {
	for name, tc := range map[string]struct{ location, region, want string }{
		"location":       {location: "europe-west4", region: "us-east5", want: "europe-west4"},
		"region":         {region: "us-east5", want: "us-east5"},
		"global default": {want: DefaultVertexLocation},
	} {
		t.Run(name, func(t *testing.T) {
			clearVertexEnv(t)
			t.Setenv("GOOGLE_CLOUD_PROJECT", "demo-project")
			t.Setenv("GOOGLE_CLOUD_LOCATION", tc.location)
			t.Setenv("GOOGLE_CLOUD_REGION", tc.region)
			var mu sync.Mutex
			var path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				path = r.URL.Path
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
			}))
			t.Cleanup(srv.Close)
			prevURL, prevResolve := vendorBaseURL, vendorResolve
			vendorBaseURL = srv.URL
			vendorResolve = &profile.Options{Auth: auth.Options{
				DetectGoogle: func(context.Context, []string) (auth.TokenSource, error) { return fakeToken{}, nil },
			}}
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
			if want := "/projects/demo-project/locations/" + tc.want + "/"; !strings.Contains(path, want) {
				t.Errorf("request path = %q, want it to contain %q", path, want)
			}
		})
	}
}
