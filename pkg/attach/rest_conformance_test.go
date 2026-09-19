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

package attach

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/auth"
)

// REST-response conformance fixtures for the attach surface.
//
// The SSE event shapes have been fixture-pinned since v1.4.0
// (capabilities_conformance_test.go); the REST endpoints' *response*
// shapes were prose in docs/attach-mode-design.md and nothing else.
// mast reported that gap upstream as core-agent#536 after mast-web's
// bundled mock invented snake_case names for the sessions list
// (session_id/app_name/user_id), passed its own tests, and rendered
// `undefined` against every real backend (mast-web#41). core-agent
// closed it with its own fixtures in #549. This is mast's side of that
// report finally coming home.
//
// It is not a file port. mast's handler set and core-agent's have
// diverged — mast's ACL route is PUT (whole-document replace) where
// theirs is PATCH, mast's StatusInfo carries four fields where theirs
// carries seven, mast has no session titles, no per-subagent events
// route and no stop-agent route, and mast has GET /usage, whose eight
// token fields only started carrying values in #356. So every fixture
// here is constructed from mast's own runtime types. What was ported is
// the practice.
//
// Versioned independently of the SSE protocol: the -v<N> suffix is a
// REST-shape version starting at 1, bumped on any wire-shape change,
// with the old fixture kept frozen. See testdata/conformance/README.md
// for the table and for which endpoints are still unpinned.

func TestConformance_RESTSessionsListV1(t *testing.T) {
	t.Parallel()
	// listSessions wraps []sessionDescriptor in a {"sessions": ...}
	// envelope. One active row (live in the in-memory registry) and one
	// idle row (persisted-only, resumes lazily on /events) — the two
	// Status values a client has to render differently, because
	// clicking into an idle one pays a resume latency.
	resp := map[string]any{
		"sessions": []sessionDescriptor{
			{
				AppName:     "mast",
				UserID:      "alice@example.com",
				SessionID:   "s-1a2b3c",
				HasEventLog: true,
				Status:      sessionStatusActive,
				// Active rows take this from Entry.LastTouchedAt(), which
				// is time.Unix(0, ns) in the daemon's local zone, so the
				// wire carries RFC 3339 with arbitrary sub-second
				// precision and a zone offset. The fixture shows that
				// shape deliberately: parse the field, don't pattern-match
				// a trailing "Z" off these examples.
				LastTouchedAt: time.Date(2026, 9, 19, 12, 34, 56, 789012345, time.FixedZone("", 2*60*60)),
			},
			{
				AppName:       "mast",
				UserID:        "alice@example.com",
				SessionID:     "s-9z8y7x",
				HasEventLog:   true,
				Status:        sessionStatusIdle,
				LastTouchedAt: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC),
			},
		},
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-sessions-list-v1.json",
		resp)
}

// TestConformance_RESTSessionsListEnvelopeIsLive guards the half the
// fixture above cannot: listSessions builds its envelope as an inline
// map literal, so a fixture written against a hand-built map agrees
// with itself no matter what the handler does. This drives the real
// handler and asserts the key set it actually emits.
func TestConformance_RESTSessionsListEnvelopeIsLive(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "mast", user: "alice@example.com", sid: "s-1a2b3c"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := &handlers{reg: reg}

	r := httptest.NewRequest(http.MethodGet, "/sessions", strings.NewReader(""))
	rr := httptest.NewRecorder()
	h.listSessions(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("listSessions: status %d, body %s", rr.Code, rr.Body.String())
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if got := keysOf(envelope); len(got) != 1 || got[0] != "sessions" {
		t.Errorf("GET /sessions envelope keys = %v, want exactly [sessions]", got)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["sessions"], &rows); err != nil {
		t.Fatalf("decode rows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	// Every field is present, including the two tagged omitempty. That
	// is not an accident of this stub: encoding/json's omitempty has no
	// effect on a struct, so last_touched_at is on the wire even when it
	// is the zero time, and so is next_wake_at on GET /status. A client
	// must read those two for the zero value rather than treat presence
	// as meaning the field applies. Asserting the full key set is what
	// keeps that true — a reader who "fixes" the tag to make the field
	// disappear fails here.
	want := []string{"app", "has_event_log", "last_touched_at", "sessionID", "status", "user"}
	if got := keysOf(rows[0]); !equalStrings(got, want) {
		t.Errorf("sessions row keys = %v, want %v", got, want)
	}
}

// TestConformance_RESTSessionsListEmptyIsArrayNotNull pins the one
// shape in this file byte-exactly, because it is the one a client
// crashes on rather than renders wrong. listSessions allocates its
// slice with make(..., 0, n), so an operator with no readable sessions
// gets {"sessions":[]} and can iterate without a nil check. Changing
// that allocation to a var declaration is a one-character edit that
// emits null, passes every other test here — the key set is identical
// — and breaks every consumer that trusted the array.
func TestConformance_RESTSessionsListEmptyIsArrayNotNull(t *testing.T) {
	t.Parallel()
	h := &handlers{reg: NewSessionRegistry()}

	r := httptest.NewRequest(http.MethodGet, "/sessions", strings.NewReader(""))
	rr := httptest.NewRecorder()
	h.listSessions(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("listSessions: status %d, body %s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"sessions":[]}` {
		t.Errorf("GET /sessions with no sessions = %s, want {\"sessions\":[]}", got)
	}
}

// TestConformance_OmitemptyDoesNothingToATimestamp is the check behind
// the claim above, because the live test cannot make it: that row's
// last_touched_at may simply have been non-zero. These two fields are
// tagged omitempty and a reader who trusts the tag will write a client
// that treats their presence as meaning they apply. Marshal the zero
// value and look.
func TestConformance_OmitemptyDoesNothingToATimestamp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		v     any
		field string
		zero  string
	}{
		{"sessions row", sessionDescriptor{AppName: "mast", SessionID: "s"}, "last_touched_at", "0001-01-01T00:00:00Z"},
		{"status", StatusInfo{State: "idle"}, "next_wake_at", "0001-01-01T00:00:00Z"},
	} {
		raw, err := json.Marshal(tc.v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		v, present := got[tc.field]
		if !present {
			t.Errorf("%s: %s was dropped when zero — omitempty started working on a struct, "+
				"which is a wire-shape change for every client reading it: %s", tc.name, tc.field, raw)
			continue
		}
		if v != tc.zero {
			t.Errorf("%s: zero %s marshalled as %v, want %q", tc.name, tc.field, v, tc.zero)
		}
	}
}

func TestConformance_RESTCreateSessionV1(t *testing.T) {
	t.Parallel()
	// POST /sessions → 201. URL is echoed from the Host header of the
	// creating request, so it is normally absolute; a client that
	// prefixes its own base onto it gets "http://h/http://h/sessions/…".
	// Use it as given.
	resp := createSessionResponse{
		AppName:   "mast",
		UserID:    "alice@example.com",
		SessionID: "s-1a2b3c",
		URL:       "http://mast.internal:8080/sessions/mast/s-1a2b3c",
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-create-session-v1.json",
		resp)
}

func TestConformance_RESTWhoAmIV1(t *testing.T) {
	t.Parallel()
	// The asserted-proxy variant, chosen because it is the one that
	// populates both omitempty fields. `source` reports what the server
	// verified, never what a header claimed, and a client must tolerate
	// values it does not know — a future authenticator adds its own.
	resp := whoAmIResponse{
		Identity: "oncall@example.com",
		Admin:    true,
		Source:   whoAmISourceAsserted,
		ProxyBy:  "incident-bot@example.com",
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-whoami-v1.json",
		resp)
}

func TestConformance_RESTStatusV1(t *testing.T) {
	t.Parallel()
	// GET /sessions/{app}/{sid}/status. The running-with-a-tool variant:
	// it is the one that populates current_tool, and it pins
	// next_wake_at's absence on a session that is not deferred.
	resp := StatusInfo{
		State:       "running",
		ModelName:   "claude-sonnet-5",
		CurrentTool: "get_k8s_pod_logs",
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-status-v1.json",
		resp)
}

func TestConformance_RESTSessionACLV1(t *testing.T) {
	t.Parallel()
	// GET /sessions/{app}/{sid}/acl, and the body of a successful PUT.
	// viewers and contributors are always present as arrays, never null,
	// so a client can append without a nil check — the opposite
	// convention to the omitempty fields elsewhere, and deliberate: an
	// owner-only ACL is what every session starts with, so walking an
	// empty one is the common path rather than an edge.
	//
	// enforced and persisted are mast-side fields with no core-agent
	// counterpart, and they are the two that stop this response being
	// misleading: an ACL that reads plausibly on a daemon that never
	// consults it, or that will not survive a restart, is worse than no
	// ACL endpoint at all.
	resp := sessionACLResponse{
		Owner:        "lookout@example.com",
		Viewers:      []string{"sre-readonly@example.com"},
		Contributors: []string{"oncall@example.com", "incident-bot@example.com"},
		Enforced:     true,
		Persisted:    true,
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-session-acl-v1.json",
		resp)
}

func TestConformance_RESTUsageV1(t *testing.T) {
	t.Parallel()
	// GET /sessions/{app}/{sid}/usage. The eight token fields were
	// declared from the day the route shipped and carried values from
	// #356 on, which makes this the fixture most worth having: a client
	// written against the old response saw five structural zeros and
	// three absent fields, and cannot tell that apart from a session
	// that genuinely spent nothing.
	//
	// The figures are the measured claude-sonnet-5 turn pkg/budget
	// prices against (28,804-token cached system block, Vertex us-east5,
	// 2026-09-13): a warming turn, then a turn that hits the entry. They
	// are arithmetically consistent with the shipped rate card, so the
	// two properties a reader most often gets wrong are both visible
	// here rather than asserted in prose — the warming turn reports its
	// cache write as UNCACHED input, because a write bills at a premium
	// over fresh input rather than a discount, and its
	// cost_usd_uncached_reference therefore sits BELOW its cost_usd. The
	// reference is a counterfactual ("what this would have cost with no
	// prompt cache at all"), not a floor.
	warmAt := time.Date(2026, 9, 19, 14, 2, 11, 0, time.UTC)
	hitAt := time.Date(2026, 9, 19, 14, 2, 47, 0, time.UTC)
	perModel := UsageTotals{
		InputTokens:              57664,
		InputTokensCached:        28804,
		InputTokensUncached:      28860,
		OutputTokens:             204,
		Turns:                    2,
		CostUSD:                  0.0799228,
		CostUSDUncachedReference: 0.117368,
	}
	resp := UsageInfo{
		Overall:  perModel,
		PerModel: map[string]UsageTotals{"claude-sonnet-5": perModel},
		PerTurn: []UsageTurn{
			{
				Turn:                     1,
				At:                       warmAt,
				Model:                    "claude-sonnet-5",
				InputTokens:              28814,
				InputTokensUncached:      28814,
				OutputTokens:             4,
				TotalTokens:              28818,
				CostUSD:                  0.07207,
				CostUSDUncachedReference: 0.057668,
			},
			{
				Turn:                     2,
				At:                       hitAt,
				Model:                    "claude-sonnet-5",
				InputTokens:              28850,
				InputTokensCached:        28804,
				InputTokensUncached:      46,
				OutputTokens:             200,
				TotalTokens:              29050,
				CostUSD:                  0.0078528,
				CostUSDUncachedReference: 0.0597,
			},
		},
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-usage-v1.json",
		resp)
}

// TestConformance_RESTUsageWarmingTurnReadsBelowItsCost states the
// claim the fixture's numbers make, as an assertion rather than as a
// comment. A later edit that "corrects" the warming row so its
// reference exceeds its cost would otherwise silently turn the fixture
// into an example of the misreading it exists to prevent.
func TestConformance_RESTUsageWarmingTurnReadsBelowItsCost(t *testing.T) {
	t.Parallel()
	var got UsageInfo
	readConformanceFixture(t, "testdata/conformance/rest-usage-v1.json", &got)
	if len(got.PerTurn) < 2 {
		t.Fatalf("fixture has %d turns, want the warm+hit pair", len(got.PerTurn))
	}
	warm, hit := got.PerTurn[0], got.PerTurn[1]
	if warm.InputTokensCached != 0 {
		t.Errorf("warming turn reports %d cached input tokens, want 0: a cache WRITE is uncached input", warm.InputTokensCached)
	}
	if warm.CostUSDUncachedReference >= warm.CostUSD {
		t.Errorf("warming turn reference $%.6f >= cost $%.6f; a cache that has not been reused has cost money and saved none",
			warm.CostUSDUncachedReference, warm.CostUSD)
	}
	if hit.CostUSDUncachedReference <= hit.CostUSD {
		t.Errorf("cache-hit turn reference $%.6f <= cost $%.6f; the hit is where the saving shows up",
			hit.CostUSDUncachedReference, hit.CostUSD)
	}
}

// TestConformance_RESTInjectV1 pins POST /sessions/{app}/{sid}/inject
// off the live handler rather than off a literal: doInject builds its
// response as an inline map, so a fixture compared against a map this
// test wrote would prove nothing about the handler.
func TestConformance_RESTInjectV1(t *testing.T) {
	t.Parallel()
	ag := &stubRegistrant{app: "mast", user: "alice@example.com", sid: "s-incident-4412"}
	reg := NewSessionRegistry()
	entry, err := reg.Register(ag)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := &handlers{reg: reg}

	body := `{"message":"second alert corroborates the first"}`
	r := httptest.NewRequest(http.MethodPost, "/sessions/mast/s-incident-4412/inject", strings.NewReader(body))
	r = r.WithContext(auth.WithCaller(context.Background(), auth.Caller{Identity: "oncall@example.com"}))
	rr := httptest.NewRecorder()
	h.doInject(rr, r, entry)
	if rr.Code != http.StatusOK {
		t.Fatalf("doInject: status %d, body %s", rr.Code, rr.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/rest-inject-v1.json",
		got)
}

// readConformanceFixture decodes a fixture back into a runtime type, so
// a test can assert on what the fixture SAYS rather than only that the
// type still marshals to it.
func readConformanceFixture(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode fixture %s: %v", path, err)
	}
}

// keysOf returns a map's keys sorted, for key-set assertions.
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
