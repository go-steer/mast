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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/auth"
)

// A resume that answers a parked mutating call is an approval, and an
// approval that cannot name an approver is not much of an audit record.
// These pin who /resume says is behind a request.

const resumeBody = `{"session_id":"s1","interrupt_id":"i1","response":{"verdict":"approve"}}`

// resumeProbe serves one POST /resume and reports the caller the
// handler saw.
func resumeProbe(t *testing.T, cfg Config, header map[string]string) (*auth.Caller, string, int) {
	t.Helper()
	var seen *auth.Caller
	var proxyBy string
	cfg.Handler = nopHandler
	cfg.ResumeHandler = func(ctx context.Context, _ ResumeRequest) error {
		if c, ok := auth.CallerFromContext(ctx); ok {
			seen = &c
		}
		proxyBy, _ = auth.ProxyByFromContext(ctx)
		return nil
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest("POST", "/resume", strings.NewReader(resumeBody))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	return seen, proxyBy, rec.Code
}

// TestResumeCaller_NoAuthenticatorNamesTheSharedCredential: the daemon's
// default is one shared bearer token, and the honest record for it is
// the token, not a person and not "anonymous".
func TestResumeCaller_NoAuthenticatorNamesTheSharedCredential(t *testing.T) {
	caller, proxyBy, code := resumeProbe(t, Config{}, nil)
	if code != 202 {
		t.Fatalf("POST /resume = %d, want 202", code)
	}
	if caller == nil {
		t.Fatal("handler saw no caller on the context; an approval would be recorded with a blank approver")
	}
	if caller.Identity != SharedCredentialIdentity {
		t.Errorf("caller identity = %q, want %q", caller.Identity, SharedCredentialIdentity)
	}
	if proxyBy != "" {
		t.Errorf("proxy = %q, want empty", proxyBy)
	}
}

func TestResumeCaller_AuthenticatedIdentityReachesTheHandler(t *testing.T) {
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
	}, nil, nil)
	caller, _, code := resumeProbe(t,
		Config{Authenticator: authn},
		map[string]string{"Authorization": "Bearer alice-token"})
	if code != 202 {
		t.Fatalf("POST /resume = %d, want 202", code)
	}
	if caller == nil || caller.Identity != "alice@example.com" {
		t.Fatalf("caller = %+v, want alice@example.com", caller)
	}
}

// TestResumeCaller_BadCredentialIsForbiddenNotDowngraded: an operator
// who configured a user table asked for attributed approvals. Falling
// back to the shared identity here would put a lie in the audit log.
func TestResumeCaller_BadCredentialIsForbiddenNotDowngraded(t *testing.T) {
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
	}, nil, nil)
	caller, _, code := resumeProbe(t,
		Config{Authenticator: authn},
		map[string]string{"Authorization": "Bearer not-alices-token"})
	if code != 403 {
		t.Fatalf("POST /resume with a bad credential = %d, want 403", code)
	}
	if caller != nil {
		t.Errorf("handler ran anyway and saw %+v; the resume was dispatched despite an unresolvable caller", caller)
	}
}

// TestResumeCaller_ProxyAssertsOnBehalfOf is the Slack path: a bot holds
// the credential and the human it is relaying for is the approver. Both
// are recorded — the effective caller and the proxy that vouched.
func TestResumeCaller_ProxyAssertsOnBehalfOf(t *testing.T) {
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
		{Identity: "sa:slack-bot", Token: "bot-token"},
	}, nil, []string{"sa:slack-bot"})
	caller, proxyBy, code := resumeProbe(t,
		Config{Authenticator: authn},
		map[string]string{
			"Authorization":           "Bearer bot-token",
			auth.HeaderAssertedCaller: "alice@example.com",
		})
	if code != 202 {
		t.Fatalf("POST /resume = %d, want 202", code)
	}
	if caller == nil || caller.Identity != "alice@example.com" {
		t.Fatalf("caller = %+v, want the asserted human", caller)
	}
	if proxyBy != "sa:slack-bot" {
		t.Errorf("proxy = %q, want sa:slack-bot — the record must keep who vouched", proxyBy)
	}
}

// TestResumeCaller_UnprivilegedProxyIsRefused: without this, any holder
// of any token could approve as anyone by setting a header.
func TestResumeCaller_UnprivilegedProxyIsRefused(t *testing.T) {
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
		{Identity: "bob@example.com", Token: "bob-token"},
	}, nil, nil)
	caller, _, code := resumeProbe(t,
		Config{Authenticator: authn},
		map[string]string{
			"Authorization":           "Bearer bob-token",
			auth.HeaderAssertedCaller: "alice@example.com",
		})
	if code != 403 {
		t.Fatalf("POST /resume asserting another identity without proxy rights = %d, want 403", code)
	}
	if caller != nil {
		t.Errorf("handler saw %+v; bob approved as alice", caller)
	}
}

// TestResumeCaller_UnknownAssertedIdentityIsRefused: a proxy may only
// assert identities the table knows, so the approver on the record is
// always someone the operator provisioned.
func TestResumeCaller_UnknownAssertedIdentityIsRefused(t *testing.T) {
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "sa:slack-bot", Token: "bot-token"},
	}, nil, []string{"sa:slack-bot"})
	_, _, code := resumeProbe(t,
		Config{Authenticator: authn},
		map[string]string{
			"Authorization":           "Bearer bot-token",
			auth.HeaderAssertedCaller: "nobody@example.com",
		})
	if code != 403 {
		t.Fatalf("POST /resume asserting an unknown identity = %d, want 403", code)
	}
}
