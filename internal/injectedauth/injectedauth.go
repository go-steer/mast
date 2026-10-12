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

// Package injectedauth supports running mast where Google credentials are
// added to outbound requests by an egress gateway instead of being held by
// the process, as in an Agent Substrate sandbox with credential injection.
//
// With MAST_GOOGLE_AUTH=injected, Google clients (Vertex AI models, MCP
// servers with google_oauth auth) skip Application Default Credentials and
// send "Authorization: Bearer placeholder". The gateway replaces the header
// with a real token, and only replaces headers that are present, so the
// placeholder is required.
package injectedauth

import (
	"context"
	"os"
	"time"

	"golang.org/x/oauth2"
)

// Placeholder is the token value sent in place of a real credential.
const Placeholder = "placeholder"

// Enabled reports whether MAST_GOOGLE_AUTH=injected is set.
func Enabled() bool { return os.Getenv("MAST_GOOGLE_AUTH") == "injected" }

// farFuture keeps clients from treating the placeholder as expired.
var farFuture = time.Now().Add(100 * 365 * 24 * time.Hour)

// TokenSource returns an oauth2 token source yielding the placeholder.
func TokenSource() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: Placeholder, TokenType: "Bearer", Expiry: farFuture})
}

// Static yields the placeholder as a bare token, for clients that take
// one as a string (core-models' Google token source).
type Static struct{}

// Token returns the placeholder.
func (Static) Token(context.Context) (string, error) { return Placeholder, nil }
