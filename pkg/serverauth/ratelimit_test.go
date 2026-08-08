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

package serverauth

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"
)

func TestNewTokenBucketLimiterValidatesConfig(t *testing.T) {
	if _, err := NewTokenBucketLimiter(0, 1); err == nil {
		t.Error("rate 0: want error, got nil")
	}
	if _, err := NewTokenBucketLimiter(-1, 1); err == nil {
		t.Error("negative rate: want error, got nil")
	}
	if _, err := NewTokenBucketLimiter(1, 0); err == nil {
		t.Error("burst 0: want error, got nil")
	}
	if _, err := NewTokenBucketLimiter(1, 1); err != nil {
		t.Errorf("valid config: unexpected error %v", err)
	}
}

// TestNewTokenBucketLimiterRejectsNonFinite pins the fail-closed guard:
// strconv.ParseFloat accepts "Inf"/"NaN", and neither compares > 0 the way
// the <= 0 check expects (both comparisons are false), so a non-finite rate
// would build a limiter that never limits — the budget guard silently off.
// Construction must refuse it instead.
func TestNewTokenBucketLimiterRejectsNonFinite(t *testing.T) {
	for _, v := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		if _, err := NewTokenBucketLimiter(v, 1); err == nil {
			t.Errorf("rate %v: want error, got nil", v)
		}
	}
}

// TestNewTokenBucketLimiterRejectsInfiniteRate pins the same fail-closed
// posture for a FINITE rate that maps to rate.Inf: rate.Inf is defined as
// Limit(math.MaxFloat64), and a Limiter at that limit admits everything
// unconditionally. Such a rate passes the IsInf guard (MaxFloat64 is
// finite) but would silently disable limiting, so construction must refuse
// it.
func TestNewTokenBucketLimiterRejectsInfiniteRate(t *testing.T) {
	if _, err := NewTokenBucketLimiter(math.MaxFloat64, 1); err == nil {
		t.Error("rate math.MaxFloat64 (== rate.Inf): want error, got nil")
	}
}

// TestTokenBucketLimiterBurstThenRefuse pins the core admission contract:
// a bucket admits up to burst immediately, then refuses with a positive
// retryAfter (and consuming no further capacity on the refusal).
func TestTokenBucketLimiterBurstThenRefuse(t *testing.T) {
	lim, err := NewTokenBucketLimiter(1, 2) // 1/s, burst 2
	if err != nil {
		t.Fatalf("NewTokenBucketLimiter: %v", err)
	}
	req := RateLimitRequest{Subject: "alice", Workload: "triage", Method: "message/send"}
	for i := 0; i < 2; i++ {
		if ok, _ := lim.Allow(context.Background(), req); !ok {
			t.Fatalf("call %d within burst: want admitted, got refused", i+1)
		}
	}
	ok, retryAfter := lim.Allow(context.Background(), req)
	if ok {
		t.Fatal("call 3 over burst: want refused, got admitted")
	}
	if retryAfter <= 0 {
		t.Errorf("refused call: want positive retryAfter, got %v", retryAfter)
	}
	// The refusal must NOT consume future capacity: because call 3 canceled
	// its reservation, call 4 waits for the *same* next token (~1s), not the
	// one after it (~2s). Without res.Cancel(), call 3's reservation would
	// push call 4's wait to ~2s — so this bound isolates the Cancel.
	_, retryAfter4 := lim.Allow(context.Background(), req)
	if retryAfter4 >= 1500*time.Millisecond {
		t.Errorf("call 4 retryAfter = %v, want < 1.5s (a refusal consumed future capacity — missing res.Cancel())", retryAfter4)
	}
}

// TestTokenBucketLimiterRefillReadmits pins the time dimension: a drained
// bucket re-admits after enough time passes for a token to refill.
func TestTokenBucketLimiterRefillReadmits(t *testing.T) {
	lim, err := NewTokenBucketLimiter(10, 1) // 10/s → refill ~100ms
	if err != nil {
		t.Fatalf("NewTokenBucketLimiter: %v", err)
	}
	req := RateLimitRequest{Subject: "alice", Workload: "triage"}
	if ok, _ := lim.Allow(context.Background(), req); !ok {
		t.Fatal("first call: want admitted, got refused")
	}
	// The two calls are microseconds apart; a token needs ~100ms to refill,
	// so the second is refused unless a >100ms scheduler stall intervenes —
	// three orders of magnitude off the sub-ms call gap, so this will not
	// flake on a loaded CI runner.
	if ok, _ := lim.Allow(context.Background(), req); ok {
		t.Fatal("immediate second call: want refused, got admitted")
	}
	time.Sleep(200 * time.Millisecond) // >> 100ms refill interval
	if ok, _ := lim.Allow(context.Background(), req); !ok {
		t.Error("call after refill window: want admitted, got refused")
	}
}

// TestTokenBucketLimiterKeyCollisionProof pins that no caller-derived value
// can smear one key field into the next: a Tenant containing the byte that
// once separated key fields ("\x00") must not collide with, or split from,
// any other (caller, workload). Two distinct callers each get their own
// burst.
func TestTokenBucketLimiterKeyCollisionProof(t *testing.T) {
	lim, err := NewTokenBucketLimiter(1, 1)
	if err != nil {
		t.Fatalf("NewTokenBucketLimiter: %v", err)
	}
	// caller "a", workload "b\x00c" vs caller "a\x00b", workload "c": a
	// concatenated "caller\x00workload" key would map both to "a\x00b\x00c".
	r1 := RateLimitRequest{Tenant: "a", Workload: "b\x00c"}
	r2 := RateLimitRequest{Tenant: "a\x00b", Workload: "c"}
	if ok, _ := lim.Allow(context.Background(), r1); !ok {
		t.Fatal("r1 first call: want admitted")
	}
	if ok, _ := lim.Allow(context.Background(), r2); !ok {
		t.Error("r2 first call: want admitted (distinct bucket), got refused — key collision")
	}
}

// TestTokenBucketLimiterConcurrent exercises the get-or-create under the
// race detector: concurrent Allow on the same and distinct keys must not
// race and must not over-admit beyond burst per key.
func TestTokenBucketLimiterConcurrent(t *testing.T) {
	lim, err := NewTokenBucketLimiter(1, 1) // burst 1
	if err != nil {
		t.Fatalf("NewTokenBucketLimiter: %v", err)
	}
	const workers = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := lim.Allow(context.Background(), RateLimitRequest{Subject: "alice", Workload: "triage"}); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Exactly one token was available; concurrency must not let more through.
	if admitted != 1 {
		t.Errorf("concurrent admits = %d, want exactly 1 (burst)", admitted)
	}
}

// TestTokenBucketLimiterKeyIsolation pins that buckets are per
// (caller, workload): a caller exhausting one workload's bucket does not
// affect another workload, another caller, and that Tenant (not Subject)
// is the caller identity when set.
func TestTokenBucketLimiterKeyIsolation(t *testing.T) {
	lim, err := NewTokenBucketLimiter(1, 1) // 1/s, burst 1
	if err != nil {
		t.Fatalf("NewTokenBucketLimiter: %v", err)
	}
	drain := func(r RateLimitRequest) {
		if ok, _ := lim.Allow(context.Background(), r); !ok {
			t.Fatalf("first call for %+v: want admitted, got refused", r)
		}
	}
	// Exhaust alice/triage.
	drain(RateLimitRequest{Subject: "alice", Workload: "triage"})
	if ok, _ := lim.Allow(context.Background(), RateLimitRequest{Subject: "alice", Workload: "triage"}); ok {
		t.Error("alice/triage second call: want refused, got admitted")
	}
	// Different workload → independent bucket.
	drain(RateLimitRequest{Subject: "alice", Workload: "deploy"})
	// Different caller → independent bucket.
	drain(RateLimitRequest{Subject: "bob", Workload: "triage"})

	// Tenant is the caller identity when set: two subjects sharing a tenant
	// share one bucket.
	drain(RateLimitRequest{Subject: "t-user-1", Tenant: "acme", Workload: "triage"})
	if ok, _ := lim.Allow(context.Background(), RateLimitRequest{Subject: "t-user-2", Tenant: "acme", Workload: "triage"}); ok {
		t.Error("acme tenant second subject: want refused (shared bucket), got admitted")
	}
}
