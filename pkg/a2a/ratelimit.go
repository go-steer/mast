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

// This file defines the A2A server's pluggable rate-limit seam
// (docs/a2a-design.md "Rate limiting"). The interface is the extension
// point AG-UI is designed to reuse (#11); TokenBucketLimiter is the built-in for simple
// deployments. Like the auth seam, this package never imports the
// runtime — the daemon builds a limiter from env and hands it to New.

package a2a

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitRequest identifies one inbound call for an admission decision.
// The server fills it from the authenticated principal and the resolved
// target workload before dispatching the turn-driving verb.
type RateLimitRequest struct {
	// Subject is the authenticated caller (Principal.Subject); empty when
	// the endpoint is unauthenticated (all such callers share one bucket).
	Subject string

	// Tenant is the caller's tenant claim (Principal.Tenant), when set.
	// When present it is the caller identity the limiter buckets on, so a
	// multi-token tenant is limited as one caller.
	Tenant string

	// Workload is the target workload (skill) the call routes to.
	Workload string

	// Method is the JSON-RPC method being admitted (e.g. "message/send").
	Method string
}

// RateLimiter admits or refuses an inbound call before the server drives
// the turn. The server calls Allow once per turn-driving request
// (message/send); cheap control-plane verbs (tasks/get, tasks/cancel)
// are not gated, so an operator can always read or cancel a task. A false
// return maps to the A2A retryable "unavailable" error (-32000) with an
// advisory Retry-After. Nil disables rate limiting. Implementations must
// be safe for concurrent use. The seam is designed for AG-UI to reuse
// when it lands (issue #11).
type RateLimiter interface {
	// Allow reports whether the request may proceed. When ok is false,
	// retryAfter is an advisory backoff hint (zero if unknown).
	Allow(ctx context.Context, req RateLimitRequest) (ok bool, retryAfter time.Duration)
}

// TokenBucketLimiter is the built-in RateLimiter: an independent
// token bucket per (caller, workload), where the caller is the request's
// Tenant if set else its Subject. Every bucket shares the same rate and
// burst. It admits a request only when a token is available immediately;
// a refused request reports the wait until the next token as retryAfter
// and does NOT consume future capacity.
//
// The bucket map grows one entry per distinct (caller, workload) seen and
// is not evicted — consistent with the daemon's per-session pools at v0.2
// single-instance scale (bounded eviction is a follow-on).
type TokenBucketLimiter struct {
	limit rate.Limit
	burst int

	mu      sync.Mutex
	buckets map[bucketKey]*rate.Limiter
}

// bucketKey identifies a token bucket by its (caller, workload) pair. A
// struct key (rather than a concatenated string) is collision-proof: no
// separator a caller-derived Subject/Tenant could contain can smear one
// field into the next — important once claim-based validators (v0.3) fill
// these from caller-influenced tokens.
type bucketKey struct {
	caller   string
	workload string
}

// NewTokenBucketLimiter builds a limiter admitting perSecond requests per
// (caller, workload) with the given burst. perSecond must be a finite
// value > 0 and burst must be >= 1.
func NewTokenBucketLimiter(perSecond float64, burst int) (*TokenBucketLimiter, error) {
	if math.IsNaN(perSecond) || math.IsInf(perSecond, 0) {
		// strconv.ParseFloat accepts "NaN"/"Inf"; a non-finite rate would
		// construct a limiter that never limits (NaN <= 0 and +Inf <= 0 are
		// both false) — fail closed on the budget guard instead.
		return nil, fmt.Errorf("a2a: rate-limit rate must be finite, got %v", perSecond)
	}
	if perSecond <= 0 {
		return nil, fmt.Errorf("a2a: rate-limit rate must be > 0, got %v", perSecond)
	}
	if rate.Limit(perSecond) >= rate.Inf {
		// rate.Inf == Limit(math.MaxFloat64); a limit at or above it makes
		// rate.Limiter admit every request unconditionally. A finite but
		// astronomically large rate would slip past the IsInf guard yet still
		// disable limiting — fail closed on the budget guard here too.
		return nil, fmt.Errorf("a2a: rate-limit rate too large (would disable limiting), got %v", perSecond)
	}
	if burst < 1 {
		return nil, fmt.Errorf("a2a: rate-limit burst must be >= 1, got %d", burst)
	}
	return &TokenBucketLimiter{
		limit:   rate.Limit(perSecond),
		burst:   burst,
		buckets: map[bucketKey]*rate.Limiter{},
	}, nil
}

// Allow implements RateLimiter.
func (l *TokenBucketLimiter) Allow(_ context.Context, req RateLimitRequest) (bool, time.Duration) {
	lim := l.bucket(req)
	// Reserve (not Allow) so a refusal can report the wait until the next
	// token. Cancel returns the reservation to the bucket so refused
	// requests do not consume future capacity.
	res := lim.Reserve()
	if !res.OK() {
		return false, 0
	}
	if d := res.Delay(); d > 0 {
		res.Cancel()
		return false, d
	}
	return true, 0
}

// bucket returns the token bucket for a request's (caller, workload),
// creating it on first use.
func (l *TokenBucketLimiter) bucket(req RateLimitRequest) *rate.Limiter {
	caller := req.Subject
	if req.Tenant != "" {
		caller = req.Tenant
	}
	key := bucketKey{caller: caller, workload: req.Workload}

	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.buckets[key]
	if !ok {
		lim = rate.NewLimiter(l.limit, l.burst)
		l.buckets[key] = lim
	}
	return lim
}
