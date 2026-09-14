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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// parksProbe serves one GET against the park routes and reports the
// request the handler saw.
func parksProbe(t *testing.T, cfg Config, path string, header map[string]string, answer func(ParksRequest) (ParksResult, error)) (*ParksRequest, *httptest.ResponseRecorder) {
	t.Helper()
	var seen *ParksRequest
	cfg.Handler = nopHandler
	if answer != nil {
		cfg.ParksHandler = func(_ context.Context, req ParksRequest) (ParksResult, error) {
			seen = &req
			return answer(req)
		}
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	return seen, rec
}

func emptyParks(ParksRequest) (ParksResult, error) { return ParksResult{}, nil }

// noParks is emptyParks as the Config hook's own type.
func noParks(context.Context, ParksRequest) (ParksResult, error) { return ParksResult{}, nil }

// The path variable is the whole difference between the two routes, and
// the only place a mux pattern typo would hide.
func TestParkRoutesKeyOnTheSessionInThePath(t *testing.T) {
	for path, want := range map[string]string{
		"/parks":              "",
		"/parks/incident-42":  "incident-42",
		"/parks/a%2Fb":        "a/b",
		"/parks/incident-42/": "",
	} {
		seen, rec := parksProbe(t, Config{}, path, nil, emptyParks)
		if path == "/parks/incident-42/" {
			// A trailing slash is not a session named "". Go's mux does
			// not match `/parks/{session}` against it, so it falls to
			// the catch-all rather than reading as a list.
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", path, rec.Code)
			}
			if seen != nil {
				t.Errorf("GET %s reached the handler as %+v; a trailing slash must not read as the list route", path, *seen)
			}
			continue
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s), want 200", path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		if seen == nil {
			t.Fatalf("GET %s never reached the handler", path)
		}
		if seen.SessionID != want {
			t.Errorf("GET %s → SessionID %q, want %q", path, seen.SessionID, want)
		}
	}
}

// An empty answer is `{"sessions":[]}`, never `{"sessions":null}`. A
// client that iterates the array should not have to nil-check a list
// that is always present in the contract.
func TestAnEmptyParkListIsAnArray(t *testing.T) {
	_, rec := parksProbe(t, Config{}, "/parks", nil, emptyParks)
	body := strings.TrimSpace(rec.Body.String())
	if strings.Contains(body, "null") {
		t.Errorf("empty park read encoded a null:\n%s", body)
	}
	var out struct {
		Sessions []SessionParks `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if out.Sessions == nil || len(out.Sessions) != 0 {
		t.Errorf("sessions = %v, want an empty array", out.Sessions)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// A park read is an approval question with a cluster change in it. The
// gate is /resume's, not /metrics'.
func TestParkReadsRefuseAnUnauthenticatedCaller(t *testing.T) {
	cfg := Config{BearerToken: "shared-token"}
	for _, path := range []string{"/parks", "/parks/s1"} {
		seen, rec := parksProbe(t, cfg, path, nil, emptyParks)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s = %d, want 401", path, rec.Code)
		}
		if seen != nil {
			t.Errorf("unauthenticated GET %s reached the handler", path)
		}
		if _, rec := parksProbe(t, cfg, path, map[string]string{"Authorization": "Bearer shared-token"}, emptyParks); rec.Code != http.StatusOK {
			t.Errorf("authenticated GET %s = %d, want 200", path, rec.Code)
		}
	}
}

// The same credential matrix /resume accepts: whoever may answer a park
// may read it. A user token that could answer but not read would leave
// a relay approving something it cannot render.
func TestAUserTokenCanReadTheParkItCanAnswer(t *testing.T) {
	for _, token := range []string{"shared-token", "alice-token", "bot-token"} {
		_, rec := parksProbe(t, injectBoth(), "/parks", map[string]string{"Authorization": "Bearer " + token}, emptyParks)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /parks with %s = %d, want 200", token, rec.Code)
		}
	}
	_, rec := parksProbe(t, injectBoth(), "/parks", map[string]string{"Authorization": "Bearer nobodys-token"}, emptyParks)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /parks with an unknown token = %d, want 401", rec.Code)
	}
}

// A daemon with no session store has no park read. 404 says so; 200
// with an empty list would tell a relay there is nothing to approve.
func TestNoHandlerMeansTheRouteIsAbsent(t *testing.T) {
	for _, path := range []string{"/parks", "/parks/s1"} {
		_, rec := parksProbe(t, Config{}, path, nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s without a ParksHandler = %d, want 404", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "session store") {
			t.Errorf("404 body does not say why:\n%s", rec.Body.String())
		}
	}
}

// Each status means something different to a relay: your session is
// gone, retry me elsewhere, or mast is broken. Collapsing them onto 500
// makes a poller retry a session that will never come back.
func TestParkReadStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"unknown session", fmt.Errorf("session %q: %w", "s1", ErrNotFound), http.StatusNotFound},
		{"refused payload", fmt.Errorf("reserved id: %w", ErrBadPayload), http.StatusBadRequest},
		{"draining", fmt.Errorf("stopping: %w", ErrUnavailable), http.StatusServiceUnavailable},
		{"store on fire", errors.New("disk i/o error"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec := parksProbe(t, Config{}, "/parks/s1", nil, func(ParksRequest) (ParksResult, error) {
				return ParksResult{}, tc.err
			})
			if rec.Code != tc.want {
				t.Errorf("GET /parks/s1 = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "" {
				t.Error("503 carries no Retry-After; a relay has nothing to back off on")
			}
			// An internal error names nothing about the store. The
			// detail goes to the daemon's log, not to a chat relay.
			if tc.want == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "disk i/o") {
				t.Errorf("500 body leaks the internal error:\n%s", rec.Body.String())
			}
		})
	}
}

// A park read is a read. Anything else on these paths is a mistake, and
// #277's rule is that a mistake gets told which verb the door takes —
// including on the path-variable route, which the catch-all sees as a
// literal string.
func TestTheParkRoutesAreReadOnly(t *testing.T) {
	s, err := New(Config{Handler: nopHandler, ParksHandler: noParks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, path := range []string{"/parks", "/parks/incident-42"} {
		rec := serve(t, s, http.MethodPost, path)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", path, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Errorf("POST %s Allow = %q, want GET", path, got)
		}
	}
}

// routeMatches is what makes the assertion above work for the second
// path. Its failure mode is silent — a too-eager wildcard would answer
// 405 for paths this server does not serve at all — so it is pinned
// directly rather than only through the route table.
func TestRouteMatchesTreatsAWildcardAsOneSegment(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"/parks/{session}", "/parks/incident-42", true},
		{"/parks/{session}", "/parks/", false},
		{"/parks/{session}", "/parks", false},
		{"/parks/{session}", "/parks/a/b", false},
		{"/parks", "/parks", true},
		{"/parks", "/parksy", false},
		{"/resume", "/parks/incident-42", false},
	} {
		if got := routeMatches(tc.pattern, tc.path); got != tc.want {
			t.Errorf("routeMatches(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// The 404 body is the server's own description of itself (#277). A
// route that is served and not listed sends an operator looking for a
// door that is already open.
func TestTheParkRoutesAreInTheRouteList(t *testing.T) {
	s, err := New(Config{Handler: nopHandler, ParksHandler: noParks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := serve(t, s, http.MethodGet, "/no-such-path").Body.String()
	for _, want := range []string{"GET   /parks\n", "GET   /parks/{session}\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("404 body does not list %q:\n%s", strings.TrimSpace(want), body)
		}
	}
}
