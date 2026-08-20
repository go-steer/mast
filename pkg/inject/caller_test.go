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

// The daemon (#198) sets both credentials: MAST_INJECT_TOKEN gates every
// route, and a users.json names the person behind a resume. They arrive
// in the same Authorization header, and before #198 that made /resume
// unreachable by either one — a user token failed the shared-token gate,
// and the shared token cleared the gate and then failed authentication.
// These pin the matrix.

// injectBoth is the daemon's shipped shape: a shared token and a table.
func injectBoth() Config {
	return Config{
		BearerToken: "shared-token",
		Authenticator: auth.NewBearerTokenAuth([]auth.User{
			{Identity: "alice@example.com", Token: "alice-token"},
			{Identity: "sa:slack-bot", Token: "bot-token"},
		}, nil, []string{"sa:slack-bot"}),
	}
}

// TestResumeCaller_UserTokenIsAdmittedAlongsideTheSharedToken is the
// regression: a person's own credential has to get past the gate, or the
// table names nobody, because nothing using it can reach the handler.
func TestResumeCaller_UserTokenIsAdmittedAlongsideTheSharedToken(t *testing.T) {
	caller, _, code := resumeProbe(t, injectBoth(),
		map[string]string{"Authorization": "Bearer alice-token"})
	if code != 202 {
		t.Fatalf("POST /resume with a user token = %d, want 202", code)
	}
	if caller == nil || caller.Identity != "alice@example.com" {
		t.Fatalf("caller = %+v, want alice@example.com", caller)
	}
}

// TestResumeCaller_SharedTokenStillWorksAndStillSaysSo: configuring a
// table must not break the emitters already resuming with the shared
// token, and what it records for them does not improve — a shared
// credential proves someone held it and nothing more.
func TestResumeCaller_SharedTokenStillWorksAndStillSaysSo(t *testing.T) {
	caller, _, code := resumeProbe(t, injectBoth(),
		map[string]string{"Authorization": "Bearer shared-token"})
	if code != 202 {
		t.Fatalf("POST /resume with the shared token = %d, want 202", code)
	}
	if caller == nil || caller.Identity != SharedCredentialIdentity {
		t.Fatalf("caller = %+v, want %q", caller, SharedCredentialIdentity)
	}
}

// TestResumeCaller_ATokenThatIsNeitherIsRefused: the fallback is to the
// shared credential specifically, not to "whatever got this far".
func TestResumeCaller_ATokenThatIsNeitherIsRefused(t *testing.T) {
	caller, _, code := resumeProbe(t, injectBoth(),
		map[string]string{"Authorization": "Bearer neither-of-them"})
	if code != 401 {
		t.Fatalf("POST /resume with an unrecognized token = %d, want 401", code)
	}
	if caller != nil {
		t.Errorf("handler saw %+v; an unrecognized credential resumed a session", caller)
	}
}

// TestResumeCaller_TheSharedTokenCannotVouchForAnyone: a credential that
// cannot name its own holder must not name someone else's. Ignoring the
// header instead would attribute the approval to a shared token while
// the relay believed it had named a human.
func TestResumeCaller_TheSharedTokenCannotVouchForAnyone(t *testing.T) {
	caller, _, code := resumeProbe(t, injectBoth(),
		map[string]string{
			"Authorization":           "Bearer shared-token",
			auth.HeaderAssertedCaller: "alice@example.com",
		})
	if code != 403 {
		t.Fatalf("POST /resume asserting a caller on the shared token = %d, want 403", code)
	}
	if caller != nil {
		t.Errorf("handler saw %+v; the shared token approved as a named person", caller)
	}
}

// TestResumeCaller_ProxyStillWorksWithASharedTokenConfigured keeps the
// relay path whole: the fallback above is reached only when the table
// rejected the credential, so a genuine proxy is unaffected by it.
func TestResumeCaller_ProxyStillWorksWithASharedTokenConfigured(t *testing.T) {
	caller, proxyBy, code := resumeProbe(t, injectBoth(),
		map[string]string{
			"Authorization":           "Bearer bot-token",
			auth.HeaderAssertedCaller: "alice@example.com",
		})
	if code != 202 {
		t.Fatalf("POST /resume = %d, want 202", code)
	}
	if caller == nil || caller.Identity != "alice@example.com" || proxyBy != "sa:slack-bot" {
		t.Fatalf("caller = %+v proxied by %q, want alice asserted by the bot", caller, proxyBy)
	}
}

// TestResumeCaller_ATableWithNoSharedTokenIsTheStrictPosture: an operator
// who wants every approval attributed gets there by not setting
// MAST_INJECT_TOKEN, and then there is no unattributed way to resume.
func TestResumeCaller_ATableWithNoSharedTokenIsTheStrictPosture(t *testing.T) {
	cfg := injectBoth()
	cfg.BearerToken = ""

	caller, _, code := resumeProbe(t, cfg, nil)
	if code != 403 {
		t.Fatalf("POST /resume with no credential = %d, want 403", code)
	}
	if caller != nil {
		t.Errorf("handler saw %+v; an unauthenticated resume was attributed", caller)
	}
	// And it is refused because it cannot be attributed, not because the
	// door is shut: a real user token still gets through.
	named, _, code := resumeProbe(t, cfg, map[string]string{"Authorization": "Bearer alice-token"})
	if code != 202 || named == nil || named.Identity != "alice@example.com" {
		t.Fatalf("POST /resume with a user token = %d caller %+v, want 202 and alice", code, named)
	}
}

// TestResume_OtherRoutesAreNotWidenedByTheTable: the table says who
// approved. It is not a second way into the daemon, and a user token
// must not open /inject or /abort.
func TestResume_OtherRoutesAreNotWidenedByTheTable(t *testing.T) {
	cfg := injectBoth()
	cfg.Handler = nopHandler
	cfg.AbortHandler = func(context.Context, AbortRequest) error { return nil }
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, route := range []struct{ path, body string }{
		{"/inject", `{"session_id":"s1","text":"hello"}`},
		{"/abort", `{"session_id":"s1","reason":"stop"}`},
	} {
		req := httptest.NewRequest("POST", route.path, strings.NewReader(route.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer alice-token")
		rec := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("POST %s with a user token = %d, want 401 — the user table widened a route it does not attribute",
				route.path, rec.Code)
		}
	}
}
