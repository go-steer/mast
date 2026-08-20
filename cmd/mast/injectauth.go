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
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/go-steer/mast/pkg/auth"
)

// Environment keys for the inject listener's user table (#198). Env
// rather than a config file because that is how every other door on
// this daemon is configured — MAST_INJECT_TOKEN, MAST_ATTACH_TOKEN,
// MAST_AGUI_TOKEN — and mast has no daemon-wide config file to put a
// key in. pkg/auth's own doc comment names core-agent's
// attach.multi_session.auth.table_file as its config home; that is
// core-agent's shape, and it has a config file.
const (
	// envInjectUsersFile points at a users.json (see auth.LoadUsersFile:
	// schema version pinned, mode 0600 or stricter enforced).
	envInjectUsersFile = "MAST_INJECT_USERS_FILE"
	// envInjectProxyIdentities is the comma-separated list of identities
	// in that table permitted to answer on someone else's behalf via
	// X-Asserted-Caller — a chat relay with an approve button is the
	// motivating case.
	envInjectProxyIdentities = "MAST_INJECT_PROXY_IDENTITIES"
)

// injectAuthenticator builds the inject listener's Authenticator from
// the environment, or returns nil when no user table is configured.
//
// Nil is the shipped default and stays honest: an approval then records
// [inject.SharedCredentialIdentity], which is what a shared bearer token
// can prove — that *someone* holding it resumed. What this function adds
// is the option of proving more.
//
// Admin identities are deliberately not read. auth.NewBearerTokenAuth
// takes them, but nothing on the inject path consults Caller.Admin —
// it gates session ACLs and peer management on the attach surface — and
// an authorization knob that nothing enforces reads as enforcement that
// is not there.
func injectAuthenticator(logger *slog.Logger, sharedToken string) (auth.Authenticator, error) {
	path := os.Getenv(envInjectUsersFile)
	proxies := splitIdentities(os.Getenv(envInjectProxyIdentities))
	if path == "" {
		// A proxy list with no table cannot be honored, and a request
		// asserting a caller would be refused by a rule the operator
		// believes they configured away. Fail at startup instead.
		if len(proxies) > 0 {
			return nil, fmt.Errorf("%s is set but %s is not; there is no user table to grant proxy rights in",
				envInjectProxyIdentities, envInjectUsersFile)
		}
		return nil, nil
	}

	users, err := auth.LoadUsersFile(path)
	if err != nil {
		return nil, fmt.Errorf("inject user table: %w", err)
	}
	authn := auth.NewBearerTokenAuth(users.Users, nil, proxies)
	for _, id := range proxies {
		if !authn.HasIdentity(id) {
			return nil, fmt.Errorf("%s names %q, which is not a row in %s", envInjectProxyIdentities, id, path)
		}
	}

	logger.Info("inject user table loaded; a resume can name a person",
		"path", path, "users", len(users.Users), "proxy_identities", len(proxies))
	if sharedToken == "" {
		// Not a warning: this is the strict posture, and an operator who
		// reached it on purpose should not be told they made a mistake.
		logger.Info("MAST_INJECT_TOKEN is not set, so /resume accepts only tokens from the user table")
	} else {
		logger.Warn("MAST_INJECT_TOKEN is set alongside the user table; a resume presenting it is still accepted "+
			"and recorded as an unattributed shared credential",
			"unset_to_require_attribution", "MAST_INJECT_TOKEN")
	}
	return authn, nil
}

// splitIdentities parses a comma-separated identity list, ignoring
// surrounding whitespace and empty entries.
func splitIdentities(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if id := strings.TrimSpace(part); id != "" {
			out = append(out, id)
		}
	}
	return out
}
