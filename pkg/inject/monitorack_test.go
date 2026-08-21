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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/auth"
)

// POST /monitor-ack (v0.5 W4.6). The route's whole reason to exist is
// that mast is the only thing in the path that authenticated the
// operator — the producer's ack surface takes an ack_by string from
// whoever calls it and cannot check it. So what these pin is where the
// identity comes from.

// ackProbe serves one POST /monitor-ack and reports what the handler
// saw: the request as decoded, the caller resolved from the credential,
// and the proxy that vouched for them.
func ackProbe(t *testing.T, cfg Config, header map[string]string, body string) (*MonitorAckRequest, *auth.Caller, string, *httptest.ResponseRecorder) {
	t.Helper()
	var seenReq *MonitorAckRequest
	var seenCaller *auth.Caller
	var proxyBy string
	cfg.Handler = nopHandler
	if cfg.MonitorAckHandler == nil {
		cfg.MonitorAckHandler = func(ctx context.Context, req MonitorAckRequest) (MonitorAckResult, error) {
			seenReq = &req
			if c, ok := auth.CallerFromContext(ctx); ok {
				seenCaller = &c
			}
			proxyBy, _ = auth.ProxyByFromContext(ctx)
			// What the daemon's acker does: attribute from the
			// credential, never from the body.
			id := ""
			if seenCaller != nil {
				id = seenCaller.Identity
			}
			return MonitorAckResult{Workload: "gke-triage", Subject: req.Subject, AckBy: id}, nil
		}
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest("POST", "/monitor-ack", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	return seenReq, seenCaller, proxyBy, rec
}

const ackBody = `{"subject":"ns/checkout/oom","reason":"known"}`

// TestMonitorAck_AttributesFromTheCredential is the whole point: the
// answer to "who silenced this" comes from what the request presented.
func TestMonitorAck_AttributesFromTheCredential(t *testing.T) {
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
	}, nil, nil)
	req, caller, _, rec := ackProbe(t,
		Config{Authenticator: authn},
		map[string]string{"Authorization": "Bearer alice-token"}, ackBody)
	if rec.Code != 200 {
		t.Fatalf("POST /monitor-ack = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if caller == nil || caller.Identity != "alice@example.com" {
		t.Fatalf("caller = %+v, want alice@example.com", caller)
	}
	if req == nil || req.Subject != "ns/checkout/oom" || req.Reason != "known" {
		t.Fatalf("handler saw %+v, want the subject and reason as posted", req)
	}
	var res MonitorAckResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	// Echoed so a relay renders the identity mast resolved rather than
	// the one it assumed. For the proxy path below, those differ.
	if res.AckBy != "alice@example.com" || res.Subject != "ns/checkout/oom" {
		t.Errorf("result = %+v, want the resolved identity echoed back", res)
	}
}

// TestMonitorAck_BodyAckByIsRefused: the plan settled that a
// body-supplied ack_by must not be honoured. Refused rather than
// ignored — silently dropping a field the client believed in produces an
// audit line naming the wrong person with no sign anything went wrong.
func TestMonitorAck_BodyAckByIsRefused(t *testing.T) {
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
	}, nil, nil)
	req, _, _, rec := ackProbe(t,
		Config{Authenticator: authn},
		map[string]string{"Authorization": "Bearer alice-token"},
		`{"subject":"ns/checkout/oom","ack_by":"bob@example.com"}`)
	if rec.Code != 400 {
		t.Fatalf("POST /monitor-ack with ack_by in the body = %d, want 400", rec.Code)
	}
	if req != nil {
		t.Errorf("handler ran anyway and saw %+v; alice acked as bob", req)
	}
	// Named, and with the way out: a relay acking for a human has a
	// supported path, and the refusal has to point at it.
	if !strings.Contains(rec.Body.String(), "ack_by") || !strings.Contains(rec.Body.String(), auth.HeaderAssertedCaller) {
		t.Errorf("refusal = %q, want it to name the field and the header that does work", rec.Body.String())
	}
}

// TestMonitorAck_ProxyAssertsTheHuman is the in-chat path: the
// switchboard holds the credential, the human pressed the button. The
// forwarded identity is the human; the relay is kept alongside.
func TestMonitorAck_ProxyAssertsTheHuman(t *testing.T) {
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
		{Identity: "sa:switchboard", Token: "bot-token"},
	}, nil, []string{"sa:switchboard"})
	_, caller, proxyBy, rec := ackProbe(t,
		Config{Authenticator: authn},
		map[string]string{
			"Authorization":           "Bearer bot-token",
			auth.HeaderAssertedCaller: "alice@example.com",
		}, ackBody)
	if rec.Code != 200 {
		t.Fatalf("POST /monitor-ack = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if caller == nil || caller.Identity != "alice@example.com" {
		t.Fatalf("caller = %+v, want the asserted human", caller)
	}
	if proxyBy != "sa:switchboard" {
		t.Errorf("proxy = %q, want the relay recorded alongside", proxyBy)
	}
}

// TestMonitorAck_AnUnprivilegedProxyIsRefused: without this, any token
// holder could mute an alert under someone else's name — and unlike a
// resume, there is no gate on the far side to catch it.
func TestMonitorAck_AnUnprivilegedProxyIsRefused(t *testing.T) {
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
		{Identity: "bob@example.com", Token: "bob-token"},
	}, nil, nil)
	req, _, _, rec := ackProbe(t,
		Config{Authenticator: authn},
		map[string]string{
			"Authorization":           "Bearer bob-token",
			auth.HeaderAssertedCaller: "alice@example.com",
		}, ackBody)
	if rec.Code != 403 {
		t.Fatalf("POST /monitor-ack asserting another identity without proxy rights = %d, want 403", rec.Code)
	}
	if req != nil {
		t.Errorf("handler saw %+v; bob acked as alice", req)
	}
}

// TestMonitorAck_SharedCredentialIsNamedHonestly: the daemon's default
// is one shared token, and the honest record for it is the token. A
// deployment that wants a person's name configures a user table.
func TestMonitorAck_SharedCredentialIsNamedHonestly(t *testing.T) {
	_, caller, _, rec := ackProbe(t, Config{}, nil, ackBody)
	if rec.Code != 200 {
		t.Fatalf("POST /monitor-ack = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if caller == nil || caller.Identity != SharedCredentialIdentity {
		t.Fatalf("caller = %+v, want %q", caller, SharedCredentialIdentity)
	}
}

// TestMonitorAck_UnauthenticatedIsRefused: the route is the only authn
// in front of a suppression, so an unauthorized POST must not reach the
// handler at all.
func TestMonitorAck_UnauthenticatedIsRefused(t *testing.T) {
	req, _, _, rec := ackProbe(t, Config{BearerToken: "shared-token"}, nil, ackBody)
	if rec.Code != 401 {
		t.Fatalf("POST /monitor-ack with no credential = %d, want 401", rec.Code)
	}
	if req != nil {
		t.Errorf("handler saw %+v; an unauthenticated ack was forwarded", req)
	}
}

// TestMonitorAck_NoHandlerIs404: a workload that declares no monitor.ack
// has nowhere to forward an ack. 404 rather than a silent 200, because a
// relay that believes it suppressed something and did not is worse than
// one that is told.
func TestMonitorAck_NoHandlerIs404(t *testing.T) {
	s, err := New(Config{Handler: nopHandler})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest("POST", "/monitor-ack", strings.NewReader(ackBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("POST /monitor-ack with no handler = %d, want 404", rec.Code)
	}
}

// TestMonitorAck_BadPayloads: a subject is what the ack is about, and an
// ack with none is a request to silence everything — which no operator
// means and mast will not infer. Unknown fields are refused for the
// reason ack_by is: a field a client believed in and mast dropped is a
// suppression that did not do what the client thinks it did.
func TestMonitorAck_BadPayloads(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no subject", `{"reason":"known"}`},
		{"blank subject", `{"subject":"   "}`},
		{"not JSON", `{{`},
		{"unknown field", `{"subject":"s","window":"4h"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _, _, rec := ackProbe(t, Config{}, nil, tc.body)
			if rec.Code != 400 {
				t.Errorf("POST /monitor-ack (%s) = %d, want 400", tc.name, rec.Code)
			}
			if req != nil {
				t.Errorf("handler saw %+v for %s", req, tc.name)
			}
		})
	}
}

// TestMonitorAck_HandlerErrorsMap: the daemon refuses a wrong-workload
// ack with ErrBadPayload, and that has to reach the relay as a 400 it
// can act on rather than a 500 it will retry.
func TestMonitorAck_HandlerErrorsMap(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"bad payload", wrapErr(ErrBadPayload), 400},
		{"conflict", wrapErr(ErrConflict), 409},
		{"unavailable", wrapErr(ErrUnavailable), 503},
		{"anything else", errors.New("the producer is down"), 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{MonitorAckHandler: func(context.Context, MonitorAckRequest) (MonitorAckResult, error) {
				return MonitorAckResult{}, tc.err
			}}
			_, _, _, rec := ackProbe(t, cfg, nil, ackBody)
			if rec.Code != tc.want {
				t.Errorf("POST /monitor-ack with %s = %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}
}

func wrapErr(sentinel error) error {
	return errWrap{sentinel}
}

type errWrap struct{ inner error }

func (e errWrap) Error() string { return "forwarding the ack: " + e.inner.Error() }
func (e errWrap) Unwrap() error { return e.inner }
