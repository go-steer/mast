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

package inject_test

import (
	"context"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/envelope"
	"github.com/go-steer/mast/pkg/inject"
)

func noopHandler(context.Context, envelope.InjectPayload) error { return nil }

// TestCheckBindPolicy enumerates the policy. The rows that matter are
// the ones that must stay refused: this is the guard #361 found missing
// on the most powerful of mast's four HTTP surfaces, and a guard is
// only as good as the case it does not wave through.
func TestCheckBindPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		addr          string
		authenticated bool
		wantErr       bool
	}{
		// The bug: mast's own default bind, with no token.
		{name: "wildcard unauthenticated", addr: ":7777", wantErr: true},
		{name: "explicit all-interfaces unauthenticated", addr: "0.0.0.0:7777", wantErr: true},
		{name: "routable address unauthenticated", addr: "10.0.0.5:7777", wantErr: true},
		{name: "ipv6 wildcard unauthenticated", addr: "[::]:7777", wantErr: true},
		{name: "hostname unauthenticated", addr: "mast.internal:7777", wantErr: true},

		// A token is the credential gate, so the bind is the
		// operator's call again.
		{name: "wildcard with a token", addr: ":7777", authenticated: true},
		{name: "routable with a token", addr: "10.0.0.5:7777", authenticated: true},

		// Loopback is the local-dev escape hatch, token or not.
		{name: "loopback unauthenticated", addr: "127.0.0.1:7777"},
		{name: "ipv6 loopback unauthenticated", addr: "[::1]:7777"},
		{name: "localhost unauthenticated", addr: "localhost:7777"},

		// An unset Listen is the embedded construction path and is
		// not judged; New applies the default after this runs.
		{name: "unset", addr: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := inject.CheckBindPolicy(tc.addr, tc.authenticated)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("CheckBindPolicy(%q, %v) = nil; want a refusal", tc.addr, tc.authenticated)
				}
				// A refusal that does not name the fix is a refusal
				// the operator works around by guessing.
				if !strings.Contains(err.Error(), "MAST_INJECT_TOKEN") {
					t.Errorf("refusal should name the token env var, got: %v", err)
				}
				if !strings.Contains(err.Error(), "127.0.0.1") {
					t.Errorf("refusal should name the loopback escape hatch, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("CheckBindPolicy(%q, %v) = %v; want nil", tc.addr, tc.authenticated, err)
			}
		})
	}
}

// TestNewRefusesUnauthenticatedNonLoopback is the #361 gate, and it
// fails against pre-#361 code, where inject.New had no bind guard at
// all and this construction succeeded.
func TestNewRefusesUnauthenticatedNonLoopback(t *testing.T) {
	t.Parallel()

	_, err := inject.New(inject.Config{Listen: "0.0.0.0:7777", Handler: noopHandler})
	if err == nil {
		t.Fatal("inject.New accepted an unauthenticated all-interfaces bind; expected a refusal")
	}
	if !strings.Contains(err.Error(), "refusing to bind non-loopback") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestNewAcceptsTheGatedAndLocalShapes pins that the guard did not
// close anything that was legitimately open: a tokened daemon on any
// interface, a loopback dev daemon, and the embedded path that passes
// no Listen at all.
func TestNewAcceptsTheGatedAndLocalShapes(t *testing.T) {
	t.Parallel()

	for _, cfg := range []inject.Config{
		{Listen: "0.0.0.0:7777", BearerToken: "tok", Handler: noopHandler},
		{Listen: "127.0.0.1:7777", Handler: noopHandler},
		{Handler: noopHandler},
	} {
		if _, err := inject.New(cfg); err != nil {
			t.Errorf("inject.New(%+v): %v", cfg, err)
		}
	}
}

// TestNewRefusesAUserTableStandingInForAToken is the restraint that
// makes the policy honest. An Authenticator gates /resume and
// /monitor-ack only — /inject, /abort and /stop still read the shared
// token — so a table with no token leaves the listener open, and
// treating it as a credential gate would let an attribution feature
// satisfy an authentication check.
func TestNewRefusesAUserTableStandingInForAToken(t *testing.T) {
	t.Parallel()

	authn := auth.NewBearerTokenAuth(
		[]auth.User{{Identity: "alice@example.com", Token: "tok_alice"}}, nil, nil)
	_, err := inject.New(inject.Config{
		Listen:        "0.0.0.0:7777",
		Authenticator: authn,
		Handler:       noopHandler,
	})
	if err == nil {
		t.Fatal("a user table was accepted as authentication for the whole listener; it gates two routes")
	}
}
