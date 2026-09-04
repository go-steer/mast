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
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/auth"
)

// The wire contract switchboard codes against (v0.5 W6.2/W6.3).
//
// switchboard's in-chat Approve/Reject and its ack button are written in
// another repo, against these two routes. mast's accountability for
// those board rows is not a feature — it is that the shape does not move
// under them, and that the identity switchboard asserts arrives where
// #194 put it.
//
// # Why a shape test rather than more behaviour tests
//
// Every field below is already exercised somewhere: caller_test.go pins
// the attribution path, monitorack_test.go pins the ack semantics,
// server_status_test.go pins the error mapping. What none of them
// notices is a field being RENAMED. A Go rename compiles, passes every
// behavioural test in this package because the tests are renamed with
// it, and breaks a client in a repo the compiler cannot see. So these
// assert the JSON names as literals — a change here is meant to be
// annoying, because it is a change somebody else has to ship.
//
// The rule for editing this file: if a name below changes, the same PR
// files an issue against switchboard. Adding an optional field is fine
// and needs no coordination; renaming or removing one is a break.

// jsonNames returns the wire names of a struct's exported fields, in
// sorted order, with omitempty and other options stripped.
func jsonNames(t *testing.T, v any) []string {
	t.Helper()
	rt := reflect.TypeOf(v)
	if rt.Kind() != reflect.Struct {
		t.Fatalf("jsonNames: %T is not a struct", v)
	}
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func assertNames(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s wire fields = %v, want %v\n"+
			"A rename here breaks switchboard, which cannot be caught by compiling this repo. "+
			"If the change is deliberate, file the issue there in the same PR.", what, got, want)
	}
}

// TestWireContract_ResumeRequest pins the body switchboard's
// Approve/Reject buttons post. session_id + interrupt_id are the keying
// an interactive callback has (they came out of the message mast
// posted); token is the v0.2 programmatic keying and is not switchboard's
// path, but it is on the same struct and a rename would still break a
// client.
func TestWireContract_ResumeRequest(t *testing.T) {
	assertNames(t, "ResumeRequest", jsonNames(t, ResumeRequest{}), []string{
		"ack_effects", "interrupt_id", "response", "session_id", "token",
	})
}

// TestWireContract_MonitorAckRequest pins the ack body — and pins that
// there is NO ack_by field on it. That absence is the contract: mast
// takes the identity from the credential, and a struct field would be a
// standing invitation to send one.
func TestWireContract_MonitorAckRequest(t *testing.T) {
	got := jsonNames(t, MonitorAckRequest{})
	assertNames(t, "MonitorAckRequest", got, []string{"reason", "subject", "workload"})
	for _, name := range got {
		if name == "ack_by" {
			t.Error("MonitorAckRequest grew an ack_by field; the identity comes from the credential, " +
				"and the route refuses a body carrying one (see handleMonitorAck)")
		}
	}
}

// TestWireContract_MonitorAckResult pins what a relay renders back into
// the chat: who mast decided the acker was, and whether somebody had
// already asked.
func TestWireContract_MonitorAckResult(t *testing.T) {
	assertNames(t, "MonitorAckResult", jsonNames(t, MonitorAckResult{}), []string{
		"ack_by", "previously_acked_at", "previously_acked_by", "subject", "workload",
	})
}

// TestWireContract_AssertedCallerHeaderName pins the header W6.3's
// approver allowlist rides on. switchboard asserts the human behind a
// button press here; mast checks the relay is allowed to and refuses
// otherwise (caller_test.go). The constant is in pkg/auth, but the
// contract is this package's routes, so it is pinned here too — a
// rename in pkg/auth would otherwise be invisible until a relay 403s.
func TestWireContract_AssertedCallerHeaderName(t *testing.T) {
	if auth.HeaderAssertedCaller != "X-Asserted-Caller" {
		t.Errorf("asserted-caller header = %q, want X-Asserted-Caller", auth.HeaderAssertedCaller)
	}
	// Canonical form matters: switchboard sets it through a Go http
	// client, but a relay written in anything else sets bytes.
	if got := http.CanonicalHeaderKey(auth.HeaderAssertedCaller); got != auth.HeaderAssertedCaller {
		t.Errorf("header constant %q is not in canonical form (%q)", auth.HeaderAssertedCaller, got)
	}
}

// TestWireContract_Routes pins the paths and the verb, and pins the one
// thing about the verb that could bite: a GET does not reach either
// handler.
//
// It used to answer 200. `GET /` was the health route and, under
// net/http's pattern matching, a catch-all for every GET — so
// `GET /resume` returned "ok" from the health handler while resuming
// nothing, which a relay author reads as success. Fixed in #277: the
// catch-all is now method-agnostic, a known path with the wrong verb
// answers 405 naming the verb it wants, and only `/` is healthy.
func TestWireContract_Routes(t *testing.T) {
	var dispatched int
	s, err := New(Config{
		Handler:       nopHandler,
		ResumeHandler: func(context.Context, ResumeRequest) error { dispatched++; return nil },
		MonitorAckHandler: func(context.Context, MonitorAckRequest) (MonitorAckResult, error) {
			dispatched++
			return MonitorAckResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tc := range []struct {
		method, path, body string
		want               int
		wantDispatch       bool
	}{
		{"POST", "/resume", resumeBody, http.StatusAccepted, true},
		{"POST", "/monitor-ack", ackBody, http.StatusOK, true},
		{"GET", "/resume", "", http.StatusMethodNotAllowed, false},
		{"GET", "/monitor-ack", "", http.StatusMethodNotAllowed, false},
		{"GET", "/", "", http.StatusOK, false},
		{"POST", "/alert", "", http.StatusNotFound, false},
	} {
		before := dispatched
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d (%s), want %d", tc.method, tc.path, rec.Code, strings.TrimSpace(rec.Body.String()), tc.want)
		}
		if got := dispatched > before; got != tc.wantDispatch {
			t.Errorf("%s %s dispatched = %v, want %v", tc.method, tc.path, got, tc.wantDispatch)
		}
	}
}

// TestWireContract_ResumeStatusCodes pins the answers a relay has to
// tell apart, because each one means something different in a chat
// window: retry me, tell the operator they typed something wrong, tell
// them somebody else already answered, or tell them this daemon is not
// the one holding their session.
func TestWireContract_ResumeStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		handlerErr error
		body       string
		want       int
		retryAfter bool
	}{
		{name: "accepted", body: resumeBody, want: http.StatusAccepted},
		{
			name: "neither keying supplied",
			body: `{"response":{"verdict":"approve"}}`,
			want: http.StatusBadRequest,
		},
		{
			name: "both keyings supplied",
			body: `{"token":"mrt_x","session_id":"s1","interrupt_id":"i1","response":{}}`,
			want: http.StatusBadRequest,
		},
		{name: "not json", body: `{`, want: http.StatusBadRequest},
		{
			name:       "the runtime refused the payload",
			handlerErr: fmt.Errorf("verdict unreadable: %w", ErrBadPayload),
			body:       resumeBody,
			want:       http.StatusBadRequest,
		},
		{
			name:       "somebody already answered",
			handlerErr: fmt.Errorf("interrupt already resumed: %w", ErrConflict),
			body:       resumeBody,
			want:       http.StatusConflict,
		},
		{
			name:       "draining",
			handlerErr: fmt.Errorf("draining: %w", ErrUnavailable),
			body:       resumeBody,
			want:       http.StatusServiceUnavailable,
			retryAfter: true,
		},
		{
			name:       "anything else",
			handlerErr: errors.New("boom"),
			body:       resumeBody,
			want:       http.StatusInternalServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(Config{
				Handler:       nopHandler,
				ResumeHandler: func(context.Context, ResumeRequest) error { return tc.handlerErr },
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req := httptest.NewRequest("POST", "/resume", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.srv.Handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("POST /resume = %d (%s), want %d", rec.Code, strings.TrimSpace(rec.Body.String()), tc.want)
			}
			if tc.retryAfter && rec.Header().Get("Retry-After") == "" {
				t.Error("503 with no Retry-After: a relay has nothing to schedule its retry against")
			}
			if tc.want == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "boom") {
				t.Error("the 500 body leaked the internal error; it is the one status a relay renders verbatim into a channel")
			}
		})
	}
}

// TestWireContract_ResumeRoundTripsAVerdict is the end-to-end shape
// check: the JSON a button press produces arrives at the runtime with
// the verdict intact and nested under `response`, not flattened.
//
// The verdict vocabulary itself (approve | reject | edit) is pinned
// where it is enforced, in cmd/mast's verdictFor — pkg/inject carries
// the payload without reading it, which is the layering this asserts.
func TestWireContract_ResumeRoundTripsAVerdict(t *testing.T) {
	var got ResumeRequest
	s, err := New(Config{
		Handler: nopHandler,
		ResumeHandler: func(_ context.Context, req ResumeRequest) error {
			got = req
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const body = `{"session_id":"incident-9","interrupt_id":"i-3",` +
		`"response":{"verdict":"edit","scope":"once","args":{"replicas":2},"note":"two is enough"},` +
		`"ack_effects":true}`
	req := httptest.NewRequest("POST", "/resume", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /resume = %d (%s), want 202", rec.Code, rec.Body.String())
	}
	if got.SessionID != "incident-9" || got.InterruptID != "i-3" || !got.AckEffects {
		t.Fatalf("handler saw %+v, want the keying and ack_effects as posted", got)
	}
	raw, err := json.Marshal(got.Response)
	if err != nil {
		t.Fatalf("re-marshal response: %v", err)
	}
	var v struct {
		Verdict string         `json:"verdict"`
		Scope   string         `json:"scope"`
		Args    map[string]any `json:"args"`
		Note    string         `json:"note"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode verdict: %v", err)
	}
	if v.Verdict != "edit" || v.Scope != "once" || v.Note != "two is enough" {
		t.Errorf("verdict = %+v, want the edit as posted", v)
	}
	if fmt.Sprint(v.Args["replicas"]) != "2" {
		t.Errorf("edited args = %v, want replicas 2 — an edit whose arguments do not survive the wire is a mutation nobody authorized", v.Args)
	}
}
