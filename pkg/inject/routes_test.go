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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// The whole of #277 in one assertion: a workload declaring
// `edge_trigger.http.path: /alert` used to get 405 on it, which reads
// as "wrong verb" and sends the operator to their client. It is a path
// this server does not serve, and now says so.
func TestADeclaredTriggerPathIs404AndNamesTheRealDoor(t *testing.T) {
	s, err := New(Config{Handler: nopHandler})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := serve(t, s, http.MethodPost, "/alert")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /alert = %d, want 404 — a path this server does not serve", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"/inject", "edge_trigger.http.path", "POST  /inject"} {
		if !strings.Contains(body, want) {
			t.Errorf("404 body does not mention %q:\n%s", want, body)
		}
	}
}

func TestAKnownPathWithTheWrongVerbSaysWhichVerb(t *testing.T) {
	s, err := New(Config{Handler: nopHandler})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := serve(t, s, http.MethodGet, "/inject")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /inject = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
	if !strings.Contains(rec.Body.String(), "takes POST") {
		t.Errorf("405 body does not name the verb:\n%s", rec.Body.String())
	}
}

func TestTheHealthCheckStillAnswers(t *testing.T) {
	s, err := New(Config{Handler: nopHandler})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := serve(t, s, http.MethodGet, "/")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("GET / = %d %q, want 200 ok", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	// A liveness probe is a GET. Anything else on / is a mistake worth
	// naming rather than a health check worth answering.
	if rec := serve(t, s, http.MethodPost, "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", rec.Code)
	}
}

// The 404 body describes THIS server. A build with no metrics handler
// must not advertise /metrics, and one with it must.
func TestTheRouteListMatchesWhatWasWired(t *testing.T) {
	plain, err := New(Config{Handler: nopHandler})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if body := serve(t, plain, http.MethodGet, "/nope").Body.String(); strings.Contains(body, "/metrics") {
		t.Errorf("a server with no metrics handler advertises /metrics:\n%s", body)
	}

	withMetrics, err := New(Config{Handler: nopHandler, Metrics: http.NotFoundHandler()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if body := serve(t, withMetrics, http.MethodGet, "/nope").Body.String(); !strings.Contains(body, "/metrics") {
		t.Errorf("a server with a metrics handler does not advertise /metrics:\n%s", body)
	}
}

// Every route in the advertised list is a route the mux actually
// serves. The list is hand-maintained next to the registrations, so the
// failure it guards against is a route added to one and not the other —
// which would make the 404 body lie in the direction that costs the
// most: telling an operator to POST somewhere that answers 404.
func TestEveryAdvertisedRouteExists(t *testing.T) {
	s, err := New(Config{Handler: nopHandler, Metrics: http.NotFoundHandler()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, rt := range s.routeList() {
		rec := serve(t, s, rt.method, rt.path)
		if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "no route") {
			t.Errorf("advertised route %s %s is not served", rt.method, rt.path)
		}
		if rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("advertised route %s %s is served under a different verb", rt.method, rt.path)
		}
	}
}
