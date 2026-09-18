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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func healthServer(t *testing.T, checks map[string]HealthCheck) *Server {
	t.Helper()
	s, err := New(Config{Handler: nopHandler, HealthChecks: checks, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func decodeHealth(t *testing.T, body string) healthResponse {
	t.Helper()
	var got healthResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body %q is not the documented JSON: %v", body, err)
	}
	return got
}

func TestHealthz_ReadyWhenTheCheckPasses(t *testing.T) {
	t.Parallel()
	s := healthServer(t, map[string]HealthCheck{
		"session_db": func(context.Context) error { return nil },
	})
	rec := serve(t, s, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	got := decodeHealth(t, rec.Body.String())
	if !got.OK || got.Checks["session_db"] != healthReady {
		t.Errorf("body = %+v, want ok with session_db ready", got)
	}
}

// The point of the whole issue: a probe that can go red. 503 is what
// takes the pod out of the Service, which is what stops it accepting
// injects it cannot durably record.
func TestHealthz_RedWhenTheCheckFails(t *testing.T) {
	t.Parallel()
	s := healthServer(t, map[string]HealthCheck{
		"session_db": func(context.Context) error { return errors.New("no such table: events") },
	})
	rec := serve(t, s, http.MethodGet, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz with a failing check = %d, want 503", rec.Code)
	}
	got := decodeHealth(t, rec.Body.String())
	if got.OK || got.Checks["session_db"] != healthFailed {
		t.Errorf("body = %+v, want not-ok with session_db failed", got)
	}
}

// The body is read by anyone who can reach the port. A database error
// names a filesystem path or a DSN host; a probe response is the last
// place that should travel.
func TestHealthz_BodyLeaksNothing(t *testing.T) {
	t.Parallel()
	s := healthServer(t, map[string]HealthCheck{
		"session_db": func(context.Context) error {
			return errors.New("unable to open database file /var/lib/mast/sessions.db: permission denied")
		},
	})
	rec := serve(t, s, http.MethodGet, "/healthz")
	body := rec.Body.String()
	for _, leak := range []string{"/var/lib/mast", "permission denied", "sessions.db"} {
		if strings.Contains(body, leak) {
			t.Errorf("health body leaks %q to an unauthenticated caller:\n%s", leak, body)
		}
	}
	// And nothing beyond the two documented keys, so a later check
	// cannot quietly add a count or an identity to a public body.
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(raw) != 2 || raw["ok"] == nil || raw["checks"] == nil {
		t.Errorf("body has keys beyond ok+checks: %v", raw)
	}
}

// Unauthenticated by construction. A bearer token is configured and the
// probe carries none: if /healthz ever acquires an auth call, this is
// the test that fails, not a deployment at 3am.
func TestHealthz_NeedsNoCredential(t *testing.T) {
	t.Parallel()
	s, err := New(Config{
		Handler:     nopHandler,
		BearerToken: "s3cret",
		Listen:      "127.0.0.1:0",
		Logger:      quietLogger(),
		HealthChecks: map[string]HealthCheck{
			"session_db": func(context.Context) error { return nil },
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rec := serve(t, s, http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz without a token = %d, want 200", rec.Code)
	}
	// The neighbouring route proves the token is actually in force, so
	// the assertion above is about /healthz and not about a server that
	// happens to have auth switched off.
	if rec := serve(t, s, http.MethodPost, "/inject"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /inject without a token = %d, want 401", rec.Code)
	}
}

// Exact path only. `/healthz/` and `/healthz/sessions` are not this
// route — a prefix match would put an unauthenticated handler in front
// of a whole subtree nobody designed.
func TestHealthz_ExactPathOnly(t *testing.T) {
	t.Parallel()
	s := healthServer(t, map[string]HealthCheck{
		"session_db": func(context.Context) error { return errors.New("down") },
	})
	for _, path := range []string{"/healthz/", "/healthz/sessions", "/healthzz"} {
		rec := serve(t, s, http.MethodGet, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (only the exact path is the probe)", path, rec.Code)
		}
	}
	// And it keeps the server's own 405 discipline for the wrong verb.
	rec := serve(t, s, http.MethodPost, "/healthz")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", rec.Code)
	}
}

// No checks configured is the in-memory daemon: up, with nothing
// durable to vouch for. Answering 200 with an empty object says that;
// answering 200 with `session_db: ready` would be a lie and answering
// 503 would hold every ephemeral deployment out of its Service.
func TestHealthz_NoChecksIsReadyAndSaysNothing(t *testing.T) {
	t.Parallel()
	s := healthServer(t, nil)
	rec := serve(t, s, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz with no checks = %d, want 200", rec.Code)
	}
	got := decodeHealth(t, rec.Body.String())
	if !got.OK || len(got.Checks) != 0 {
		t.Errorf("body = %+v, want ok with an empty checks object", got)
	}
}

// One line per transition, not per probe. kubelet's default cadence is
// every 10s; a line per probe is ~8,600 a day per pod, and the
// transition that matters ends up buried in the noise it generated.
func TestHealthz_LogsTransitionsNotProbes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	healthy := true
	s, err := New(Config{
		Handler: nopHandler,
		Logger:  slog.New(slog.NewTextHandler(&buf, nil)),
		HealthChecks: map[string]HealthCheck{
			"session_db": func(context.Context) error {
				if healthy {
					return nil
				}
				return errors.New("no such table: events")
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for range 5 {
		serve(t, s, http.MethodGet, "/healthz")
	}
	healthy = false
	for range 5 {
		serve(t, s, http.MethodGet, "/healthz")
	}
	healthy = true
	for range 5 {
		serve(t, s, http.MethodGet, "/healthz")
	}

	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(line, "health check") {
			lines++
		}
	}
	if lines != 3 {
		t.Errorf("logged %d health lines over 15 probes and 3 states, want 3:\n%s", lines, buf.String())
	}
	// The detail the body must not carry has to be somewhere, or a red
	// probe is undiagnosable.
	if !strings.Contains(buf.String(), "no such table: events") {
		t.Errorf("the failing check's error never reached the log:\n%s", buf.String())
	}
}

// A second failure reason is a second transition even though the
// verdict did not change: "the database came back and then broke
// differently" is exactly the sequence an operator needs to see.
func TestHealthz_ANewFailureReasonIsANewTransition(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	reason := "no such table: events"
	s, err := New(Config{
		Handler: nopHandler,
		Logger:  slog.New(slog.NewTextHandler(&buf, nil)),
		HealthChecks: map[string]HealthCheck{
			"session_db": func(context.Context) error { return errors.New(reason) },
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	serve(t, s, http.MethodGet, "/healthz")
	serve(t, s, http.MethodGet, "/healthz")
	reason = "database is locked"
	serve(t, s, http.MethodGet, "/healthz")

	if got := strings.Count(buf.String(), "health check not ready"); got != 2 {
		t.Errorf("logged %d not-ready lines, want 2 (one per distinct reason):\n%s", got, buf.String())
	}
}

// The check runs on the request's context, so a probe that the kubelet
// gave up on does not leave a query running behind it.
func TestHealthz_ChecksSeeTheRequestContext(t *testing.T) {
	t.Parallel()
	got := make(chan error, 1)
	s := healthServer(t, map[string]HealthCheck{
		"session_db": func(ctx context.Context) error {
			got <- ctx.Err()
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil).WithContext(ctx))
	if err := <-got; err == nil {
		t.Error("the check did not see the abandoned request's cancellation; a probe the caller gave up on would keep reading")
	}
}

// /healthz is in the 404 body's route list, so a caller who mistypes it
// is told the right door exists.
func TestHealthz_IsListedAsARoute(t *testing.T) {
	t.Parallel()
	s := healthServer(t, nil)
	rec := serve(t, s, http.MethodGet, "/healthzz")
	if !strings.Contains(rec.Body.String(), "/healthz") {
		t.Errorf("404 body does not list /healthz:\n%s", rec.Body.String())
	}
}
