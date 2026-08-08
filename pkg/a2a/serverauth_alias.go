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

// This file re-exports the request-admission seams that were hoisted to
// pkg/serverauth (#84) so a single validator/limiter instance can authorize
// and admit both the A2A (this package) and AG-UI (pkg/agui) surfaces.
//
// The implementations moved; the a2a.* names stay. Because these are type
// aliases (not new types), an instance built as serverauth.* satisfies an
// a2a.* parameter and vice versa — so existing callers (cmd/mast, the shipped
// a2a tests) keep using a2a.Principal / a2a.NewStaticBearerValidator / etc.
// unchanged, and the daemon can hand one validator to both a2a.New and
// agui.New.

package a2a

import "github.com/go-steer/mast/pkg/serverauth"

// Auth + rate-limit seam types, re-exported as aliases.
type (
	// Principal is the authenticated caller a TokenValidator resolves a
	// bearer token to. See serverauth.Principal.
	Principal = serverauth.Principal

	// TokenValidator resolves a bearer token to a Principal. See
	// serverauth.TokenValidator.
	TokenValidator = serverauth.TokenValidator

	// StaticBearerValidator validates against a fixed token→Principal map.
	// See serverauth.StaticBearerValidator.
	StaticBearerValidator = serverauth.StaticBearerValidator

	// RateLimiter admits or refuses an inbound turn-driving call. See
	// serverauth.RateLimiter.
	RateLimiter = serverauth.RateLimiter

	// RateLimitRequest identifies one inbound call for an admission
	// decision. See serverauth.RateLimitRequest.
	RateLimitRequest = serverauth.RateLimitRequest

	// TokenBucketLimiter is the built-in RateLimiter. See
	// serverauth.TokenBucketLimiter.
	TokenBucketLimiter = serverauth.TokenBucketLimiter
)

// ErrInvalidToken marks an unrecognized bearer token; the server maps it to
// HTTP 401. Re-exported so errors.Is(err, a2a.ErrInvalidToken) keeps
// matching after the hoist (it is the same error value as
// serverauth.ErrInvalidToken).
var ErrInvalidToken = serverauth.ErrInvalidToken

// Constructors, re-exported as func values (a Go func cannot be
// type-aliased). Callers use a2a.NewStaticBearerValidator /
// a2a.NewTokenBucketLimiter unchanged.
var (
	NewStaticBearerValidator = serverauth.NewStaticBearerValidator
	NewTokenBucketLimiter    = serverauth.NewTokenBucketLimiter
)
