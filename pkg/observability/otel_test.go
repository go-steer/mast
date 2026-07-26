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
