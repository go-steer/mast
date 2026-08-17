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

// Originally derived from go-steer/core-agent@b1101f99bec3b74ad41f2a938822c9bb9bbca072:pkg/mcp/lifecycle.go
// (googleAuthTransport + Google-OAuth token wiring in transportFor).
// Adapted for mast's simpler single-server spike: no header-transport
// composition, no stdio, no otelhttp wrapping (yet). Bumped from
// c5efbb9e when b1101f9's transport-wrap hunk was taken; mast lands it
// in newHTTPToolset rather than transportFor, and the transport itself
// is errbody.go.

package mcp

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// googleAuthTransport injects "Authorization: Bearer <token>" from an
// oauth2.TokenSource on every request. Generic over the source so the
// type can back both OAuth access tokens (google.FindDefaultCredentials)
// and OIDC ID tokens (idtoken.NewTokenSource) — both return
// oauth2.TokenSource.
type googleAuthTransport struct {
	base   http.RoundTripper
	source oauth2.TokenSource
}

func (t *googleAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.source.Token()
	if err != nil {
		return nil, fmt.Errorf("mcp: fetch Google auth token: %w", err)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	return t.base.RoundTrip(clone)
}

// newGoogleAuthTransport returns an http.RoundTripper that injects a
// Google OAuth access token sourced from Application Default Credentials
// (ADC) on every request. Scopes must be non-empty — see the note on
// core-agent's GoogleOAuthAuth about why there is no implicit default.
//
// A RoundTripper rather than a finished *http.Client, because the caller
// composes it with jsonRPCErrorTransport before handing a client to the
// MCP SDK.
//
// Fails fast: a token is pre-fetched at construction time so misconfig
// (no ADC on the host, missing scope grant, unreachable metadata
// server) surfaces at server-init rather than on the first tool call.
func newGoogleAuthTransport(ctx context.Context, name string, scopes []string) (http.RoundTripper, error) {
	if len(scopes) == 0 {
		return nil, fmt.Errorf("mcp: %q: at least one OAuth scope is required", name)
	}
	creds, err := google.FindDefaultCredentials(ctx, scopes...)
	if err != nil {
		return nil, fmt.Errorf(
			"mcp: %q: load Google default credentials: %w "+
				"(run `gcloud auth application-default login` or ensure the "+
				"metadata server is reachable)", name, err)
	}
	if _, err := creds.TokenSource.Token(); err != nil {
		return nil, fmt.Errorf(
			"mcp: %q: initial Google OAuth token fetch: %w", name, err)
	}
	return &googleAuthTransport{
		base:   http.DefaultTransport,
		source: creds.TokenSource,
	}, nil
}
