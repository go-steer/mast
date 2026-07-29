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
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"golang.org/x/oauth2/google"
)

// Env vars consulted by NewVertex when project / region are not
// supplied explicitly via VertexOptions. Names match Anthropic SDK
// conventions; the GCP-standard fallbacks let the same env that drives
// Vertex Gemini also drive Vertex Anthropic.
const (
	EnvVertexProject = "ANTHROPIC_VERTEX_PROJECT_ID"
	EnvVertexRegion  = "CLOUD_ML_REGION"
)

// Default Vertex region for Claude. Most current Anthropic Vertex
// deployments live in us-east5; override per call site as needed.
const DefaultVertexRegion = "us-east5"

// VertexOptions configures NewVertex.
type VertexOptions struct {
	// Project is the GCP project ID. Empty falls back to
	// ANTHROPIC_VERTEX_PROJECT_ID, then GOOGLE_CLOUD_PROJECT (shared
	// with Gemini Vertex). A set field always wins over env.
	Project string

	// Region is the Vertex region serving Claude. Empty falls back to
	// CLOUD_ML_REGION, then GOOGLE_CLOUD_LOCATION, then
	// DefaultVertexRegion. A set field always wins over env.
	Region string

	// CacheSystem enables prompt caching on the last system block.
	// See Options.CacheSystem.
	CacheSystem bool

	// BuiltinTools toggles Anthropic's server-side built-in tools.
	// See Options.BuiltinTools.
	BuiltinTools BuiltinTools
}

// NewVertex constructs a Provider that talks to Claude via Google
// Vertex AI. Project must resolve (options or env); region falls back
// to DefaultVertexRegion. Authentication uses Application Default
// Credentials (run `gcloud auth application-default login`, or set
// GOOGLE_APPLICATION_CREDENTIALS, or rely on workload identity in
// production).
//
// We deliberately load credentials via google.FindDefaultCredentials
// ourselves and pass them to vertex.WithCredentials — vertex.WithGoogleAuth
// panics on missing creds, which we don't want at startup.
func NewVertex(ctx context.Context, opts VertexOptions) (*Provider, error) {
	project, region, err := resolveVertexTarget(opts)
	if err != nil {
		return nil, err
	}
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("anthropic-vertex: load default credentials: %w (run `gcloud auth application-default login`)", err)
	}
	return &Provider{
		name:        VertexProviderName,
		client:      anthropic.NewClient(vertex.WithCredentials(ctx, region, project, creds)),
		cacheSystem: opts.CacheSystem,
		builtins:    opts.BuiltinTools,
	}, nil
}

// resolveVertexTarget resolves the (project, region) pair NewVertex
// dials. Explicit VertexOptions fields win; project falls back to
//  1. ANTHROPIC_VERTEX_PROJECT_ID
//  2. GOOGLE_CLOUD_PROJECT (shared with Gemini Vertex)
//
// and region to
//  1. CLOUD_ML_REGION
//  2. GOOGLE_CLOUD_LOCATION (shared with Gemini Vertex)
//  3. DefaultVertexRegion
//
// Region therefore always resolves; only a missing project errors.
func resolveVertexTarget(opts VertexOptions) (project, region string, err error) {
	project = opts.Project
	if project == "" {
		project = firstNonEmpty(os.Getenv(EnvVertexProject), os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}
	if project == "" {
		return "", "", fmt.Errorf("anthropic-vertex: project is required (set VertexOptions.Project or the %s env var)", EnvVertexProject)
	}
	region = opts.Region
	if region == "" {
		region = firstNonEmpty(os.Getenv(EnvVertexRegion), os.Getenv("GOOGLE_CLOUD_LOCATION"), DefaultVertexRegion)
	}
	return project, region, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
