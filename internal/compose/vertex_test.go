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
	"reflect"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// clearVertexEnv unsets every variable the two Gemini backends read, so
// a case declares its whole environment rather than inheriting the
// developer's. t.Setenv also fails the test if it ever runs in
// parallel, which is what we want around process-global state.
func clearVertexEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GOOGLE_GENAI_USE_VERTEXAI",
		"GOOGLE_CLOUD_PROJECT",
		"GOOGLE_CLOUD_LOCATION",
		"GOOGLE_CLOUD_REGION",
	} {
		t.Setenv(k, "")
	}
}

// TestGeminiClientConfig_VertexAlias covers the point of the alias: the
// backend is named in the config rather than inferred from the
// environment, so a deployment whose credential is the service
// account's ADC needs no GOOGLE_GENAI_USE_VERTEXAI.
func TestGeminiClientConfig_VertexAlias(t *testing.T) {
	clearVertexEnv(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "demo-project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "us-central1")

	cfg, err := geminiClientConfig(ProviderVertex)
	if err != nil {
		t.Fatalf("geminiClientConfig(vertex) = err %v, want a config", err)
	}
	if cfg.Backend != genai.BackendVertexAI {
		t.Errorf("Backend = %v, want %v (the alias must not depend on GOOGLE_GENAI_USE_VERTEXAI)", cfg.Backend, genai.BackendVertexAI)
	}
	if cfg.Project != "demo-project" {
		t.Errorf("Project = %q, want %q", cfg.Project, "demo-project")
	}
	if cfg.Location != "us-central1" {
		t.Errorf("Location = %q, want %q", cfg.Location, "us-central1")
	}
}

// TestGeminiClientConfig_LocationFallbacks pins the order: the genai
// name first, then the older regional name genai also accepts, then
// genai's own default. Mast resolves it rather than leaving it empty so
// that the location a run used is a value it passed, not one filled in
// inside the SDK.
func TestGeminiClientConfig_LocationFallbacks(t *testing.T) {
	tests := []struct {
		name     string
		location string
		region   string
		want     string
	}{
		{"location wins", "europe-west4", "us-east5", "europe-west4"},
		{"region when location is unset", "", "us-east5", "us-east5"},
		{"default when neither is set", "", "", DefaultVertexLocation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearVertexEnv(t)
			t.Setenv("GOOGLE_CLOUD_PROJECT", "demo-project")
			t.Setenv("GOOGLE_CLOUD_LOCATION", tt.location)
			t.Setenv("GOOGLE_CLOUD_REGION", tt.region)

			cfg, err := geminiClientConfig(ProviderVertex)
			if err != nil {
				t.Fatalf("geminiClientConfig(vertex) = err %v, want a config", err)
			}
			if cfg.Location != tt.want {
				t.Errorf("Location = %q, want %q", cfg.Location, tt.want)
			}
		})
	}
}

// TestGeminiClientConfig_MissingProject is the error this alias exists
// to replace. Without it the run reaches genai, which fails the Vertex
// path by dumping a ClientConfig struct — or, worse, never gets there
// because the empty config sent it to the API-key backend to ask for a
// key the deployment deliberately does not have.
func TestGeminiClientConfig_MissingProject(t *testing.T) {
	clearVertexEnv(t)

	_, err := geminiClientConfig(ProviderVertex)
	if err == nil {
		t.Fatal("geminiClientConfig(vertex) with no project = nil error, want a refusal")
	}
	for _, want := range []string{"GOOGLE_CLOUD_PROJECT", "Application Default Credentials"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestGeminiClientConfig_EnvPathUnchanged is the compatibility half:
// every invocation that worked before the alias existed must still hand
// genai an empty config and let it decide. A populated config here
// would change the backend selection of every deployment that sets the
// env var, which is the one thing this change must not do.
func TestGeminiClientConfig_EnvPathUnchanged(t *testing.T) {
	for _, provider := range []string{"", ProviderGemini, "anthropic-vertex"} {
		clearVertexEnv(t)
		t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "true")
		t.Setenv("GOOGLE_CLOUD_PROJECT", "demo-project")

		cfg, err := geminiClientConfig(provider)
		if err != nil {
			t.Fatalf("geminiClientConfig(%q) = err %v, want a config", provider, err)
		}
		if !reflect.DeepEqual(*cfg, genai.ClientConfig{}) {
			t.Errorf("geminiClientConfig(%q) = %+v, want an empty config (genai's env-driven selection)", provider, *cfg)
		}
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

	if _, err := BuildModel(t.Context(), ProviderVertex, "gemini-3.5-flash"); err == nil ||
		!strings.Contains(err.Error(), "GOOGLE_CLOUD_PROJECT") {
		t.Errorf("BuildModel(vertex, gemini-3.5-flash) with no project: err = %v, want the project guidance", err)
	}
}

// TestTierModelName_VertexResolves closes the loop the tier table had
// left open: pkg/taskclass has carried a "vertex" family since the
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
