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

package observability

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

func TestSetupOTelNoOpWithoutEnv(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	shutdown, enabled, err := SetupOTel(context.Background())
	if err != nil {
		t.Fatalf("SetupOTel: %v", err)
	}
	if enabled {
		t.Error("SetupOTel enabled without OTEL_EXPORTER_OTLP_* env")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown: %v", err)
	}
}

func TestSetupOTelEnabledWithEndpoint(t *testing.T) {
	// The gRPC exporter dials lazily; installing it with an endpoint
	// that never answers must still succeed at setup time.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	shutdown, enabled, err := SetupOTel(context.Background())
	if err != nil {
		t.Fatalf("SetupOTel: %v", err)
	}
	if !enabled {
		t.Error("SetupOTel not enabled despite OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	// Shutdown flushes toward the dead endpoint; a transport error is
	// acceptable, a hang is not (bounded by the context).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

// The bug this pins is invisible inside any single build: mast's semconv
// import and the SDK's own agreed, so the merge succeeded, right up until
// a version bump made them disagree and the daemon stopped starting. The
// only way to see it from one build is to hand the merge a base whose
// schema URL is not mast's — which is what a future SDK release is.
//
// So these two versions are deliberately not real ones. A real pair would
// have to be updated on someone else's release schedule, and a test that
// needs maintaining to keep failing is a test that will be made to pass.
func TestResourceMergesWithAnySchemaURL(t *testing.T) {
	for _, schemaURL := range []string{
		"https://opentelemetry.io/schemas/1.0.0",
		"https://opentelemetry.io/schemas/99.0.0",
	} {
		t.Run(schemaURL, func(t *testing.T) {
			base := resource.NewWithAttributes(schemaURL,
				attribute.String("telemetry.sdk.name", "opentelemetry"))

			got, err := mastResource(base)
			if err != nil {
				t.Fatalf("mastResource against base schema %s: %v\n"+
					"mast must not declare a schema URL of its own here: the "+
					"SDK's moves with the SDK, and a merge across two different "+
					"ones fails, which makes SetupOTel return an error and the "+
					"daemon refuse to start", schemaURL, err)
			}
			// The merged resource keeps the *base's* schema URL, which is
			// the SDK's. Asserting this is the difference between a
			// resource that merges because it is schemaless and one that
			// merges because it quietly dropped the schema.
			if got.SchemaURL() != schemaURL {
				t.Errorf("merged schema URL = %q, want the base's %q",
					got.SchemaURL(), schemaURL)
			}
			var name string
			for _, kv := range got.Attributes() {
				if kv.Key == "service.name" {
					name = kv.Value.AsString()
				}
			}
			if name != "mast" {
				t.Errorf("service.name = %q, want \"mast\"; the attribute is "+
					"the only reason this merge happens at all", name)
			}
		})
	}
}
