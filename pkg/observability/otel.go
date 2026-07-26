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
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// SetupOTel installs the global OTLP trace exporter + W3C propagator
// when standard OTel env config asks for it (OTEL_EXPORTER_OTLP_ENDPOINT
// or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT). Endpoint, headers, protocol
// details, etc. are all read from the environment by the exporter — mast
// adds nothing beyond a service.name resource default.
//
// mast does not open custom spans in v0.1: ADK v2's runner emits the
// unified span tree (session/turn/node/tool), and mast only decorates.
// This function only makes that tree leave the process.
//
// Returns a shutdown func (flushes the batch exporter) and whether
// export was enabled. When the env vars are absent it is a no-op:
// shutdown is non-nil and trivially succeeds.
func SetupOTel(ctx context.Context) (shutdown func(context.Context) error, enabled bool, err error) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" &&
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return noop, false, nil
	}

	exp, err := otlptracegrpc.New(ctx) // endpoint/headers/TLS from OTEL_* env
	if err != nil {
		return noop, false, fmt.Errorf("otlp trace exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName("mast")))
	if err != nil {
		return noop, false, fmt.Errorf("otel resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, true, nil
}
