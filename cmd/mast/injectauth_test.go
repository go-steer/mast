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

package main

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/auth"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeUsersFile drops a valid users.json under t.TempDir() at the mode
// LoadUsersFile insists on.
func writeUsersFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write users file: %v", err)
	}
	return path
}

const twoUsers = `{"version":1,"users":[
  {"identity":"alice@example.com","token":"alice-token"},
  {"identity":"sa:slack-bot","token":"bot-token"}
]}`

// TestInjectAuthenticator_UnsetIsNil: the table is opt-in, and its
// absence has to leave the daemon exactly as it shipped.
func TestInjectAuthenticator_UnsetIsNil(t *testing.T) {
	t.Setenv(envInjectUsersFile, "")
	t.Setenv(envInjectProxyIdentities, "")

	authn, err := injectAuthenticator(quietLogger(), "shared-token")
	if err != nil {
		t.Fatalf("injectAuthenticator: %v", err)
	}
	if authn != nil {
		t.Errorf("authenticator = %T, want nil when no user table is configured", authn)
	}
}

// TestInjectAuthenticator_LoadsTheTable checks the env actually reaches
// the authenticator, by asking it to authenticate a token from the file.
func TestInjectAuthenticator_LoadsTheTable(t *testing.T) {
	t.Setenv(envInjectUsersFile, writeUsersFile(t, twoUsers))
	t.Setenv(envInjectProxyIdentities, "sa:slack-bot")

	authn, err := injectAuthenticator(quietLogger(), "")
	if err != nil {
		t.Fatalf("injectAuthenticator: %v", err)
	}
	if authn == nil {
		t.Fatal("authenticator is nil; the configured table would never name anyone")
	}
	req := httptest.NewRequest("POST", "/resume", nil)
	req.Header.Set("Authorization", "Bearer alice-token")
	caller, err := authn.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate with a token from the file: %v", err)
	}
	if caller.Identity != "alice@example.com" {
		t.Errorf("identity = %q, want alice@example.com", caller.Identity)
	}
}

// TestInjectAuthenticator_ProxyRightsAreGranted: the proxy list is the
// only reason a relay can approve on someone's behalf, so a silent
// failure to apply it looks like a permissions bug at 3am.
func TestInjectAuthenticator_ProxyRightsAreGranted(t *testing.T) {
	t.Setenv(envInjectUsersFile, writeUsersFile(t, twoUsers))
	t.Setenv(envInjectProxyIdentities, " sa:slack-bot , ")

	authn, err := injectAuthenticator(quietLogger(), "")
	if err != nil {
		t.Fatalf("injectAuthenticator: %v", err)
	}
	// Proxy rights are consulted by the inject server through this
	// optional interface; if the list did not reach the authenticator,
	// the relay's asserted caller is refused with no clue why.
	proxier, ok := authn.(interface{ CanProxyAs(auth.Caller) bool })
	if !ok {
		t.Fatalf("authenticator %T does not report proxy rights, so the proxy list is inert", authn)
	}
	for identity, want := range map[string]bool{
		"sa:slack-bot":      true,
		"alice@example.com": false,
	} {
		req := httptest.NewRequest("POST", "/resume", nil)
		req.Header.Set("Authorization", "Bearer "+map[string]string{
			"sa:slack-bot": "bot-token", "alice@example.com": "alice-token",
		}[identity])
		caller, err := authn.Authenticate(req)
		if err != nil {
			t.Fatalf("Authenticate as %s: %v", identity, err)
		}
		if got := proxier.CanProxyAs(caller); got != want {
			t.Errorf("CanProxyAs(%s) = %v, want %v", identity, got, want)
		}
	}
}

// TestInjectAuthenticator_ProxiesWithoutATableIsAStartupError: honoring
// half the configuration would refuse requests by a rule the operator
// believes they configured away.
func TestInjectAuthenticator_ProxiesWithoutATableIsAStartupError(t *testing.T) {
	t.Setenv(envInjectUsersFile, "")
	t.Setenv(envInjectProxyIdentities, "sa:slack-bot")

	authn, err := injectAuthenticator(quietLogger(), "")
	if err == nil {
		t.Fatalf("injectAuthenticator = %v, nil error; proxy rights were configured into nothing", authn)
	}
	if !strings.Contains(err.Error(), envInjectUsersFile) {
		t.Errorf("error %q does not name %s, so it does not say what to set", err, envInjectUsersFile)
	}
}

// TestInjectAuthenticator_UnknownProxyIdentityIsAStartupError: a typo
// here grants nothing and reports nothing at runtime — the relay just
// starts getting 403s.
func TestInjectAuthenticator_UnknownProxyIdentityIsAStartupError(t *testing.T) {
	t.Setenv(envInjectUsersFile, writeUsersFile(t, twoUsers))
	t.Setenv(envInjectProxyIdentities, "sa:slackbot")

	_, err := injectAuthenticator(quietLogger(), "")
	if err == nil {
		t.Fatal("injectAuthenticator returned nil error for a proxy identity that is not in the table")
	}
	if !strings.Contains(err.Error(), "sa:slackbot") {
		t.Errorf("error %q does not quote the offending identity", err)
	}
}

// TestInjectAuthenticator_BadTableIsAStartupError: LoadUsersFile's mode
// and schema checks are the point of using it, so they must abort the
// daemon rather than degrade it to no table at all.
func TestInjectAuthenticator_BadTableIsAStartupError(t *testing.T) {
	path := writeUsersFile(t, twoUsers)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Setenv(envInjectUsersFile, path)
	t.Setenv(envInjectProxyIdentities, "")

	authn, err := injectAuthenticator(quietLogger(), "")
	if err == nil {
		t.Fatalf("injectAuthenticator = %v, nil error; a world-readable token file was accepted", authn)
	}
	if authn != nil {
		t.Errorf("authenticator = %T alongside an error, want nil", authn)
	}
}

func TestSplitIdentities(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"only separators", " , , ", nil},
		{"one", "sa:bot", []string{"sa:bot"}},
		{"trimmed", " a@x.com , sa:bot ", []string{"a@x.com", "sa:bot"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitIdentities(tc.raw); !slices.Equal(got, tc.want) {
				t.Errorf("splitIdentities(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
