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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/envelope"
)

// TestErrorStatusMapping pins the sentinel→status contract (#65): a
// draining daemon answers 503 + Retry-After with a generic body (not
// the error text — leak hygiene), a content rejection answers 400
// with the reason, and anything else stays a generic 500.
func TestErrorStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantRetry  bool
		wantInBody string
	}{
		{"unavailable bare", ErrUnavailable, http.StatusServiceUnavailable, true, "retry against the replacement"},
		{"unavailable wrapped", fmt.Errorf("gate: %w", ErrUnavailable), http.StatusServiceUnavailable, true, "retry against the replacement"},
		{"bad payload wrapped", fmt.Errorf("uid %q reserved: %w", "x:mast-ops", ErrBadPayload), http.StatusBadRequest, false, "reserved"},
		{"other error", fmt.Errorf("model exploded"), http.StatusInternalServerError, false, "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(Config{
				Listen:  "127.0.0.1:0",
				Handler: func(context.Context, envelope.InjectPayload) error { return tc.err },
				ResumeHandler: func(context.Context, ResumeRequest) error {
					return tc.err
				},
				AbortHandler: func(context.Context, AbortRequest) error {
					return tc.err
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, route := range []struct{ path, body string }{
				{"/inject", `{"kind":"Event","reason":"r","namespace":"n","name":"p","uid":"u","cluster":"c","message":"m"}`},
				{"/resume", `{"session_id":"s","interrupt_id":"i","response":{}}`},
				{"/abort", `{"session_id":"s"}`},
			} {
				if route.path == "/abort" && tc.wantStatus == http.StatusServiceUnavailable {
					// Abort is deliberately NOT drain-gated (a marker
					// write is legitimate during termination); the
					// unavailable sentinel never flows through it.
					continue
				}
				req := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(route.body))
				rec := httptest.NewRecorder()
				srv.srv.Handler.ServeHTTP(rec, req)
				if rec.Code != tc.wantStatus {
					t.Errorf("%s %s: status = %d, want %d (body %q)", route.path, tc.name, rec.Code, tc.wantStatus, rec.Body.String())
				}
				if tc.wantRetry && rec.Header().Get("Retry-After") == "" {
					t.Errorf("%s %s: missing Retry-After", route.path, tc.name)
				}
				if !tc.wantRetry && rec.Header().Get("Retry-After") != "" {
					t.Errorf("%s %s: unexpected Retry-After on %d", route.path, tc.name, rec.Code)
				}
				if !strings.Contains(rec.Body.String(), tc.wantInBody) {
					t.Errorf("%s %s: body %q missing %q", route.path, tc.name, rec.Body.String(), tc.wantInBody)
				}
				// Leak hygiene: the 503 body must never echo handler
				// error text; the 500 body must stay generic.
				if tc.wantStatus != http.StatusBadRequest && strings.Contains(rec.Body.String(), "gate:") {
					t.Errorf("%s %s: body leaked handler error text: %q", route.path, tc.name, rec.Body.String())
				}
			}
		})
	}
}
