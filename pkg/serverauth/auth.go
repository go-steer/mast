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

// Package serverauth holds the request-admission seams shared by mast's
// network server surfaces (A2A — pkg/a2a; AG-UI — pkg/agui): pluggable
// bearer authentication (TokenValidator → Principal, with per-surface scope
// checks) and pluggable rate limiting (RateLimiter, in ratelimit.go). Both
// were first built for the A2A server (#78) and hoisted here so a single
// validator or limiter instance authenticates and admits across every
// surface (#84).
//
// Like the surfaces that consume it, this package never imports the runtime:
// the daemon builds a validator/limiter from configuration and hands it to
// each server's New. It depends only on the standard library and
// golang.org/x/time, so it stays slim-embed-safe.
package serverauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"strings"
)

// Principal is the authenticated caller a TokenValidator resolves a bearer
// token to. Scopes gate per-skill / per-workload access. Tenant, when set,
// is the caller identity the rate limiter buckets on
// (RateLimitRequest.Tenant).
//
// Tenant does NOT yet drive session isolation: ADK v2.1.0's IsolationScope
// is an event/task-level field (the workflow finish_task machinery), not a
// session-create or tenant seam (session.CreateRequest carries no scope).
// Multi-tenant session isolation is deferred pending an ADK session-scope
// seam or a mast-side user-namespacing design (docs/a2a-design.md
// "Multi-tenancy").
type Principal struct {
	Subject string
	Scopes  []string
	Tenant  string
}

// HasScope reports whether the principal carries want. A nil principal
// carries no scopes.
func (p *Principal) HasScope(want string) bool {
	if p == nil {
		return false
	}
	for _, s := range p.Scopes {
		if s == want {
			return true
		}
	}
	return false
}

// TokenValidator resolves a bearer token to a Principal. It returns
// ErrInvalidToken for a token it does not recognize (→ HTTP 401); any other
// error is treated as a validator fault (→ HTTP 500). Built-in:
// StaticBearerValidator. JWT/JWKS, Google IAM, and OAuth2 introspection
// validators are v0.3 (docs/a2a-design.md "Auth model") — the interface
// ships now as the extension point.
type TokenValidator interface {
	Validate(ctx context.Context, token string) (*Principal, error)
}

// ErrInvalidToken marks an unrecognized bearer token; server surfaces map it
// to HTTP 401.
var ErrInvalidToken = errors.New("serverauth: invalid or unknown bearer token")

// StaticBearerValidator validates against a fixed token→Principal map — the
// "static bearer tokens (for simple deployments)" validator from
// docs/a2a-design.md. It compares in constant time across every configured
// token so neither a miss nor a value mismatch leaks timing.
type StaticBearerValidator struct {
	principals map[string]*Principal
}

// NewStaticBearerValidator builds a validator from a token→Principal map. At
// least one non-empty token with a non-nil principal is required.
func NewStaticBearerValidator(tokens map[string]*Principal) (*StaticBearerValidator, error) {
	if len(tokens) == 0 {
		return nil, errors.New("serverauth: static bearer validator needs at least one token")
	}
	m := make(map[string]*Principal, len(tokens))
	for tok, p := range tokens {
		if tok == "" {
			return nil, errors.New("serverauth: static bearer validator: empty token")
		}
		if p == nil {
			return nil, errors.New("serverauth: static bearer validator: nil principal")
		}
		m[tok] = p
	}
	return &StaticBearerValidator{principals: m}, nil
}

// Validate resolves token to its principal, or ErrInvalidToken. It iterates
// every entry with a constant-time compare so total time does not depend on
// which (if any) token matched.
func (v *StaticBearerValidator) Validate(_ context.Context, token string) (*Principal, error) {
	var match *Principal
	for tok, p := range v.principals {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1 {
			match = p
		}
	}
	if match == nil {
		return nil, ErrInvalidToken
	}
	return match, nil
}

// IsLoopbackAddr reports whether a TCP listen address binds only a loopback
// interface. Conservative by design: an empty host (":7780"), the wildcards
// "0.0.0.0"/"::", and any hostname other than "localhost" all count as
// NON-loopback — when in doubt, treat the bind as network-reachable so a
// bind-policy check errs toward refusing. Server surfaces use it to refuse an
// unauthenticated bind beyond loopback. (pkg/attach keeps its own copy,
// predating this package.)
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
