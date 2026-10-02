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

// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818

package anthropic

import (
	"context"
	"strings"
	"testing"
)

func clearVertexEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("CLOUD_ML_REGION", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "")
}

func TestNewVertex_RequiresProject(t *testing.T) {
	clearVertexEnv(t)
	_, err := NewVertex(context.Background(), VertexOptions{Region: "us-east5"})
	if err == nil || !strings.Contains(err.Error(), "project is required") {
		t.Fatalf("expected project-required error, got %v", err)
	}
}

func TestResolveVertexTarget_OptionsWin(t *testing.T) {
	// Options fields must win over env even when env is populated.
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "env-project")
	t.Setenv("CLOUD_ML_REGION", "env-region")

	project, region, err := resolveVertexTarget(VertexOptions{
		Project: "my-project",
		Region:  "us-east5",
	})
	if err != nil {
		t.Fatalf("resolveVertexTarget: %v", err)
	}
	if project != "my-project" || region != "us-east5" {
		t.Errorf("resolved = %q/%q, want my-project/us-east5 (options win over env)", project, region)
	}
}

func TestResolveVertexTarget_HonorsEnvFallbacks(t *testing.T) {
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "from-env")
	t.Setenv("CLOUD_ML_REGION", "europe-west4")

	// No option fields — should pick up env vars.
	project, region, err := resolveVertexTarget(VertexOptions{})
	if err != nil {
		t.Fatalf("resolveVertexTarget: %v", err)
	}
	if project != "from-env" || region != "europe-west4" {
		t.Errorf("resolved = %q/%q, want from-env/europe-west4", project, region)
	}
}

func TestResolveVertexTarget_GCPStandardEnvFallbacks(t *testing.T) {
	// The GCP-standard vars shared with Gemini Vertex kick in when the
	// Anthropic-specific ones are unset.
	clearVertexEnv(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "gcp-project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "us-central1")

	project, region, err := resolveVertexTarget(VertexOptions{})
	if err != nil {
		t.Fatalf("resolveVertexTarget: %v", err)
	}
	if project != "gcp-project" || region != "us-central1" {
		t.Errorf("resolved = %q/%q, want gcp-project/us-central1", project, region)
	}
}

func TestResolveVertexTarget_DefaultRegion(t *testing.T) {
	// Region always resolves: with no explicit region anywhere,
	// DefaultVertexRegion is the final fallback (so the source's
	// "region is required" error path no longer exists).
	clearVertexEnv(t)

	_, region, err := resolveVertexTarget(VertexOptions{Project: "my-project"})
	if err != nil {
		t.Fatalf("resolveVertexTarget: %v", err)
	}
	if region != DefaultVertexRegion {
		t.Errorf("region = %q, want the %q default", region, DefaultVertexRegion)
	}
}

func TestResolveVertexTarget_MissingProjectErrors(t *testing.T) {
	clearVertexEnv(t)
	t.Setenv("CLOUD_ML_REGION", "us-east5")

	_, _, err := resolveVertexTarget(VertexOptions{})
	if err == nil || !strings.Contains(err.Error(), "project is required") {
		t.Fatalf("expected project-required error, got %v", err)
	}
}

func TestNewVertex_ReachesCredentialLoad(t *testing.T) {
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "from-env")
	t.Setenv("CLOUD_ML_REGION", "europe-west4")

	// No option fields — should pick up env vars and reach the ADC
	// load. We're testing the constructor wiring, not the GCP creds
	// load; ADC missing is the expected outcome on most CI machines.
	_, err := NewVertex(context.Background(), VertexOptions{})
	if err != nil && !strings.Contains(err.Error(), "load default credentials") {
		t.Fatalf("env fallback path should reach the creds load step, got %v", err)
	}
}
