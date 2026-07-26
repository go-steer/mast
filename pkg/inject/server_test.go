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

package inject

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/envelope"
	"github.com/go-steer/mast/pkg/observability"
)

func nopHandler(context.Context, envelope.InjectPayload) error { return nil }

func TestMetricsRouteServesRegistry(t *testing.T) {
	obs := observability.New()
	obs.TurnComplete("gke-triage", observability.OutcomeOK)

	s, err := New(Config{
		Handler: nopHandler,
		Metrics: obs.Handler(),
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "mast_turns_total") {
		t.Errorf("scrape output missing mast_turns_total; got:\n%s", body)
	}
}

func TestMetricsRouteAbsentWhenUnconfigured(t *testing.T) {
	s, err := New(Config{Handler: nopHandler})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	// Falls through to the GET / health handler ("ok"), not a scrape.
	if strings.Contains(rec.Body.String(), "mast_") {
		t.Errorf("unexpected metric output without Metrics configured: %s", rec.Body.String())
	}
}
