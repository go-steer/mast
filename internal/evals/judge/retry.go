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

package judge

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"
)

// Why this exists (#239).
//
// The judge tier's failure mode is not a low score — that reports and
// does not gate. It is a row that never ran, which makes the board
// incomplete and the night red. On 2026-08-21 three of thirty-one rows
// were lost to `Error 429 ... RESOURCE_EXHAUSTED` from Vertex, and the
// tier made no attempt to get them back, so a provider-side quota blip
// presented as a broken measurement of mast.
//
// The asymmetry that hid this is worth stating, because it is invisible
// from mast's side of the interface: anthropic-sdk-go retries 429s and
// 5xxs twice of its own accord, and google.golang.org/genai returns
// APIError{Code: 429} straight out of api_client.go with no retry at
// all. So the same corpus, on the same night, was measuring two
// different amounts of resilience depending on which model it named.
// Retrying at model.LLM — the one interface both providers arrive
// through — is what makes the two boards comparable.
//
// This does not soften the gate. An incomplete board still fails; the
// point is to stop handing it an incomplete board over something a
// three-second wait fixes.

// retryableCodes are the HTTP statuses worth waiting out.
//
// Deliberately short. 429 is the quota blip this was written for and
// 503 is the provider restarting something; both are the provider
// saying "not now" rather than "not ever". 500 is excluded: an internal
// error that reproduces is a real result, and retrying it three times
// only spends money to reach the same row three times slower. 4xx other
// than 429 are the request's own fault and will fail identically
// forever.
var retryableCodes = map[int]bool{429: true, 503: true}

// defaultRetryBackoff is the wait before each retry. Four attempts
// total, spread over about half a minute.
//
// Sized against what a Vertex per-region quota window actually is:
// quota refills on the order of a minute, so a sub-second retry storm
// would burn the budget inside one exhausted window and report the same
// failure faster. It is also bounded rather than open-ended because a
// nightly with a 90-minute timeout and thirty-one sequential metered
// rows cannot afford to wait out a real outage — it should fail and say
// the board is short.
//
// No jitter. There is exactly one caller, running scenarios one at a
// time; jitter exists to desynchronize a fleet, and here it would only
// make the test nondeterministic.
var defaultRetryBackoff = []time.Duration{3 * time.Second, 9 * time.Second, 27 * time.Second}

// RetryingLLM wraps a model so a transient provider error costs a wait
// rather than a row.
type RetryingLLM struct {
	inner   adkmodel.LLM
	backoff []time.Duration
	// sleep is time.Sleep in production and a recorder in tests. It
	// returns the context's error if the wait was interrupted.
	sleep func(ctx context.Context, d time.Duration) error
	// onRetry announces a wait before it is served. Optional, but the
	// harness passes one: a 27-second pause nobody narrated is
	// indistinguishable from a hung run in a nightly's progress log.
	onRetry func(attempt int, wait time.Duration, err error)

	mu      sync.Mutex
	retries int
	waited  time.Duration
}

// Retrying returns m wrapped in a retry policy. A nil backoff means
// [defaultRetryBackoff]; a test passes a faster one rather than
// spending the real schedule.
//
// It returns model.LLM rather than *RetryingLLM on purpose. A nil model
// has to come back as a nil *interface*, so that the constructors'
// `m == nil` guards still fire — returning a typed nil pointer would
// make NewRig accept it and move the failure to a dereference with no
// context attached. Retries are read back with [RetriesOf] rather than
// off the concrete type for the same reason.
func Retrying(m adkmodel.LLM, backoff []time.Duration, onRetry func(attempt int, wait time.Duration, err error)) adkmodel.LLM {
	if m == nil {
		return nil
	}
	if backoff == nil {
		backoff = defaultRetryBackoff
	}
	return &RetryingLLM{inner: m, backoff: backoff, sleep: sleepCtx, onRetry: onRetry}
}

// RetriesOf reports what m spent waiting out transient provider errors,
// or zero for a model that is not wrapped.
func RetriesOf(m adkmodel.LLM) (count int, waited time.Duration) {
	r, ok := m.(*RetryingLLM)
	if !ok || r == nil {
		return 0, 0
	}
	return r.Retries()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Name reports the wrapped model's name unchanged.
//
// Unchanged is load-bearing rather than lazy: the board prints the
// model under test, the cost check prices calls by model name, and
// pkg/budget meters against it. A wrapper that renamed the model would
// make all three describe a model nobody can buy.
func (r *RetryingLLM) Name() string { return r.inner.Name() }

// Retries reports how many calls were retried and how long was spent
// waiting.
//
// Reported, not swallowed. A retry nobody can see turns a provider
// under sustained pressure into a green board, which is the failure
// where a measurement quietly stops measuring — so the harness prints
// this whenever it is non-zero.
func (r *RetryingLLM) Retries() (count int, waited time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.retries, r.waited
}

// GenerateContent calls the wrapped model, retrying a transient failure
// that happened before the model said anything.
//
// "Before the model said anything" is the whole safety argument. A
// stream that already yielded a response has already handed the caller
// content; replaying the call would yield that content twice, and ADK
// assembles those yields into one turn. So the retry window closes the
// instant the first response is yielded, and a mid-stream 429 is
// reported like any other error. In the tier's own configuration this
// costs nothing — the rig runs StreamingModeNone, where the turn is a
// single yield — but the wrapper is not allowed to assume its caller.
func (r *RetryingLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for attempt := 0; ; attempt++ {
			var (
				yielded bool
				failure error
			)
			for resp, err := range r.inner.GenerateContent(ctx, req, stream) {
				if err != nil && !yielded {
					// Hold it: it is only an error to report if the
					// retry below decides not to take it.
					failure = err
					break
				}
				yielded = true
				if !yield(resp, err) {
					return
				}
			}
			if failure == nil {
				return
			}
			wait, ok := r.next(ctx, attempt, failure)
			if !ok {
				yield(nil, failure)
				return
			}
			if r.onRetry != nil {
				r.onRetry(attempt+1, wait, failure)
			}
			if err := r.sleep(ctx, wait); err != nil {
				// The wait was cancelled. Report the provider's error
				// rather than the context's: the caller wants to know
				// why the call failed, not why we stopped waiting.
				yield(nil, failure)
				return
			}
			r.record(wait)
		}
	}
}

// next decides whether attempt's failure earns another try, and how
// long to wait first.
func (r *RetryingLLM) next(ctx context.Context, attempt int, err error) (time.Duration, bool) {
	if attempt >= len(r.backoff) {
		return 0, false
	}
	// A cancelled run is not a transient provider error, and retrying it
	// would spend the backoff schedule discovering that three more times.
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, false
	}
	if !isRetryable(err) {
		return 0, false
	}
	return r.backoff[attempt], true
}

func (r *RetryingLLM) record(waited time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retries++
	r.waited += waited
}

// isRetryable reports whether err is the provider saying "not now".
//
// Typed, for both providers the tier can be pointed at:
// google.golang.org/genai returns APIError by value and ADK's Gemini
// path wraps it with %w; anthropic-sdk-go returns *anthropic.Error.
// Either way errors.As reaches the status code without reading English.
//
// There is no string fallback on purpose. Matching "429" or "resource
// exhausted" in a message would also match a model *describing* one —
// the corpus is thirty-one Kubernetes incidents, several of them about
// exhausted resources, and the grader is handed those responses
// verbatim. A classifier that can be fooled by its own payload is worse
// than one that misses a provider we have not met yet: a provider whose
// errors this cannot read simply gets no retries, which is exactly
// today's behaviour.
func isRetryable(err error) bool {
	var geminiErr genai.APIError
	if errors.As(err, &geminiErr) {
		return retryableCodes[geminiErr.Code]
	}
	var anthropicErr *anthropic.Error
	if errors.As(err, &anthropicErr) {
		return retryableCodes[anthropicErr.StatusCode]
	}
	return false
}
