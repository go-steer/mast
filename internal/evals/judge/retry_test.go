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
	"fmt"
	"iter"
	"net/http"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/mast/internal/evals"
)

// vertex429 is the error the 2026-08-21 nightly actually received,
// reproduced through the wrapping ADK's Gemini path applies to it
// (model/gemini/gemini.go: "failed to call model: %w"). Constructed
// this way rather than as a bare APIError so the test would notice if
// the classifier ever regressed to reading the message text — the
// string it would have to match is here, one %w away.
func vertex429() error {
	return fmt.Errorf("failed to call model: %w", genai.APIError{
		Code:    429,
		Status:  "RESOURCE_EXHAUSTED",
		Message: "Resource exhausted. Please try again later.",
	})
}

// flakyModel replays one outcome per call.
type flakyModel struct {
	name string
	// turns[i] is what the i-th call does: yield each response in order,
	// then fail with err if it is non-nil.
	turns []flakyTurn
	calls int
}

type flakyTurn struct {
	yields []*adkmodel.LLMResponse
	err    error
}

func (m *flakyModel) Name() string {
	if m.name == "" {
		return "scripted"
	}
	return m.name
}

func (m *flakyModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	n := m.calls
	m.calls++
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if n >= len(m.turns) {
			yield(nil, fmt.Errorf("flakyModel: call %d is off the end of the script", n+1))
			return
		}
		t := m.turns[n]
		for _, r := range t.yields {
			if !yield(r, nil) {
				return
			}
		}
		if t.err != nil {
			yield(nil, t.err)
		}
	}
}

func textResponse(s string) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{Content: genai.NewContentFromText(s, genai.RoleModel)}
}

// testRetrying builds a wrapper whose waits are recorded rather than
// served, so the schedule can be asserted without spending it.
func testRetrying(inner adkmodel.LLM) (*RetryingLLM, *[]time.Duration) {
	var slept []time.Duration
	r := &RetryingLLM{
		inner:   inner,
		backoff: defaultRetryBackoff,
		sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}
	return r, &slept
}

// drain collects everything the wrapper yields.
func drain(t *testing.T, r *RetryingLLM) ([]*adkmodel.LLMResponse, error) {
	t.Helper()
	var (
		out []*adkmodel.LLMResponse
		err error
	)
	for resp, e := range r.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if e != nil {
			err = e
			continue
		}
		out = append(out, resp)
	}
	return out, err
}

// TestA429BeforeTheModelSaysAnythingCostsAWaitNotARow is #239's whole
// point: on 2026-08-21 three of thirty-one corpus rows were lost to
// this error, and a lost row is what makes the nightly red.
func TestA429BeforeTheModelSaysAnythingCostsAWaitNotARow(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{err: vertex429()},
		{yields: []*adkmodel.LLMResponse{textResponse("CRITICAL: the pod is OOMKilled")}},
	}}
	r, slept := testRetrying(inner)

	got, err := drain(t, r)
	if err != nil {
		t.Fatalf("GenerateContent returned %v, want the retry to have succeeded", err)
	}
	if len(got) != 1 || got[0].Content.Parts[0].Text != "CRITICAL: the pod is OOMKilled" {
		t.Fatalf("got %d response(s) %+v, want the second attempt's answer", len(got), got)
	}
	if inner.calls != 2 {
		t.Errorf("called the model %d time(s), want 2", inner.calls)
	}
	if want := []time.Duration{defaultRetryBackoff[0]}; len(*slept) != 1 || (*slept)[0] != want[0] {
		t.Errorf("waited %v, want %v", *slept, want)
	}
	count, waited := r.Retries()
	if count != 1 || waited != defaultRetryBackoff[0] {
		t.Errorf("Retries() = (%d, %v), want (1, %v)", count, waited, defaultRetryBackoff[0])
	}
}

// TestTheRetryScheduleIsBoundedAndThenReportsTheProvidersError. A
// nightly with a 90-minute timeout and thirty-one sequential metered
// rows cannot wait out a real outage; it has to fail and say the board
// is short.
func TestTheRetryScheduleIsBoundedAndThenReportsTheProvidersError(t *testing.T) {
	turns := make([]flakyTurn, len(defaultRetryBackoff)+1)
	for i := range turns {
		turns[i] = flakyTurn{err: vertex429()}
	}
	inner := &flakyModel{turns: turns}
	r, slept := testRetrying(inner)

	got, err := drain(t, r)
	if len(got) != 0 {
		t.Errorf("yielded %d response(s), want none", len(got))
	}
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 429 {
		t.Fatalf("final error = %v, want the provider's 429 rather than something of our own", err)
	}
	if want := len(defaultRetryBackoff) + 1; inner.calls != want {
		t.Errorf("called the model %d time(s), want %d (one attempt per backoff step, plus the first)", inner.calls, want)
	}
	if len(*slept) != len(defaultRetryBackoff) {
		t.Fatalf("waited %v, want the whole schedule %v", *slept, defaultRetryBackoff)
	}
	for i, d := range *slept {
		if d != defaultRetryBackoff[i] {
			t.Errorf("wait %d = %v, want %v", i, d, defaultRetryBackoff[i])
		}
	}
}

// TestAnErrorThatWillFailIdenticallyForeverIsNotRetried. A 400 is the
// request's own fault; spending the schedule on it only reaches the
// same row half a minute later.
func TestAnErrorThatWillFailIdenticallyForeverIsNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a bad request", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT"}},
		{"an expired credential", genai.APIError{Code: 403, Status: "PERMISSION_DENIED"}},
		{"an internal error that reproduces", genai.APIError{Code: 500, Status: "INTERNAL"}},
		{"an error the classifier cannot read", errors.New("connection reset by peer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &flakyModel{turns: []flakyTurn{{err: tc.err}}}
			r, slept := testRetrying(inner)

			if _, err := drain(t, r); err == nil {
				t.Fatal("GenerateContent succeeded, want the error passed through")
			}
			if inner.calls != 1 {
				t.Errorf("called the model %d time(s), want 1", inner.calls)
			}
			if len(*slept) != 0 {
				t.Errorf("waited %v, want no wait at all", *slept)
			}
		})
	}
}

// TestA503IsRetried. The provider restarting something is the other
// "not now", and the corpus should survive it for the same reason.
func TestA503IsRetried(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{err: genai.APIError{Code: 503, Status: "UNAVAILABLE"}},
		{yields: []*adkmodel.LLMResponse{textResponse("ok")}},
	}}
	r, _ := testRetrying(inner)

	if _, err := drain(t, r); err != nil {
		t.Fatalf("GenerateContent returned %v, want the retry to have succeeded", err)
	}
	if inner.calls != 2 {
		t.Errorf("called the model %d time(s), want 2", inner.calls)
	}
}

// TestBothProvidersAre429AwareThroughTheirOwnErrorType. The corpus runs
// against Gemini and Claude on alternating nightlies, and the whole
// reason the 2026-08-21 gap was invisible is that only one of the two
// SDKs retries on its own. A classifier that reads one of them puts the
// asymmetry back.
func TestBothProvidersAre429AwareThroughTheirOwnErrorType(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"gemini 429", vertex429(), true},
		{"gemini 400", genai.APIError{Code: 400}, false},
		{"anthropic 429", fmt.Errorf("wrapped: %w", anthropicErr(429)), true},
		{"anthropic 503", anthropicErr(503), true},
		{"anthropic 401", anthropicErr(401), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Reports tc.name rather than the error: *anthropic.Error's
			// Error() dereferences the http.Request and Response it was
			// built from, so formatting a hand-made one turns a failing
			// assertion into a panic in the assertion.
			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// anthropicErr builds the SDK's error with the request and response it
// would carry in the wild, because Error() reads both.
func anthropicErr(status int) error {
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		panic(err)
	}
	return &anthropic.Error{
		StatusCode: status,
		Request:    req,
		Response:   &http.Response{StatusCode: status},
	}
}

// TestTheClassifierCannotBeFooledByThePayloadItIsGrading. The corpus is
// thirty-one Kubernetes incidents, several of them literally about
// exhausted resources, and the grader is handed those responses
// verbatim. A substring classifier would retry a parse failure whose
// message quotes the model.
func TestTheClassifierCannotBeFooledByThePayloadItIsGrading(t *testing.T) {
	err := fmt.Errorf("judge: grade LC-05: no JSON object in reply: %q",
		"CRITICAL: the namespace quota is exhausted — Error 429, Status: RESOURCE_EXHAUSTED on every admission")
	if isRetryable(err) {
		t.Error("a grader error quoting a model's answer was classified as a provider 429")
	}
}

// TestAFailureAfterTheModelStartedTalkingIsNotRetried. Replaying a call
// that already yielded would hand the caller that content twice, and
// ADK assembles the yields into one turn.
func TestAFailureAfterTheModelStartedTalkingIsNotRetried(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{yields: []*adkmodel.LLMResponse{textResponse("CRITICAL: the pod")}, err: vertex429()},
		{yields: []*adkmodel.LLMResponse{textResponse("should never be reached")}},
	}}
	r, slept := testRetrying(inner)

	got, err := drain(t, r)
	if err == nil {
		t.Fatal("GenerateContent succeeded, want the mid-stream error reported")
	}
	if len(got) != 1 || got[0].Content.Parts[0].Text != "CRITICAL: the pod" {
		t.Errorf("got %+v, want the one partial response, delivered once", got)
	}
	if inner.calls != 1 {
		t.Errorf("called the model %d time(s), want 1 — a started stream cannot be replayed", inner.calls)
	}
	if len(*slept) != 0 {
		t.Errorf("waited %v, want no wait", *slept)
	}
	count, _ := r.Retries()
	if count != 0 {
		t.Errorf("Retries() counted %d, want 0", count)
	}
}

// TestACancelledRunIsNotRetried. Ctrl-C, or the nightly's timeout,
// should stop rather than spend the schedule discovering that three
// more times.
func TestACancelledRunIsNotRetried(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{{err: vertex429()}, {err: vertex429()}}}
	r, slept := testRetrying(inner)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var errs []error
	for _, e := range r.GenerateContent(ctx, &adkmodel.LLMRequest{}, false) {
		if e != nil {
			errs = append(errs, e)
		}
	}
	if len(errs) != 1 {
		t.Fatalf("yielded %d error(s), want exactly 1", len(errs))
	}
	if inner.calls != 1 {
		t.Errorf("called the model %d time(s), want 1", inner.calls)
	}
	if len(*slept) != 0 {
		t.Errorf("waited %v on a cancelled context, want no wait", *slept)
	}
}

// TestAnInterruptedWaitReportsWhyTheCallFailedNotWhyWeStoppedWaiting.
// The reader of a red board wants the 429, not "context canceled".
func TestAnInterruptedWaitReportsWhyTheCallFailedNotWhyWeStoppedWaiting(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{{err: vertex429()}, {err: vertex429()}}}
	r := &RetryingLLM{
		inner:   inner,
		backoff: defaultRetryBackoff,
		sleep:   func(context.Context, time.Duration) error { return context.Canceled },
	}

	_, err := drain(t, r)
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 429 {
		t.Fatalf("final error = %v, want the provider's 429", err)
	}
	if inner.calls != 1 {
		t.Errorf("called the model %d time(s), want 1", inner.calls)
	}
	count, waited := r.Retries()
	if count != 0 || waited != 0 {
		t.Errorf("Retries() = (%d, %v), want (0, 0) — an unserved wait is not a retry", count, waited)
	}
}

// TestAConsumerThatStopsEarlyStopsTheModel. ADK's runner breaks out of
// the sequence on its own conditions; a wrapper that kept calling after
// that would spend money on turns nobody reads.
func TestAConsumerThatStopsEarlyStopsTheModel(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{yields: []*adkmodel.LLMResponse{textResponse("one"), textResponse("two")}},
	}}
	r, _ := testRetrying(inner)

	var seen int
	for range r.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("saw %d response(s) after breaking, want 1", seen)
	}
	if inner.calls != 1 {
		t.Errorf("called the model %d time(s), want 1", inner.calls)
	}
}

// TestTheWrapperDoesNotRenameTheModel. The board prints the model under
// test, J-cost-tier prices calls by model name, and pkg/budget meters
// against it; a renamed model would make all three describe something
// nobody can buy.
func TestTheWrapperDoesNotRenameTheModel(t *testing.T) {
	r := Retrying(&flakyModel{name: "gemini-3.7-flash"}, nil, nil)
	if got := r.Name(); got != "gemini-3.7-flash" {
		t.Errorf("Name() = %q, want the wrapped model's own name", got)
	}
}

// TestRetriesOfAnUnwrappedModelIsZero, so a caller reading the counter
// does not have to know whether wrapping happened.
func TestRetriesOfAnUnwrappedModelIsZero(t *testing.T) {
	count, waited := RetriesOf(&flakyModel{})
	if count != 0 || waited != 0 {
		t.Errorf("RetriesOf(unwrapped) = (%d, %v), want (0, 0)", count, waited)
	}
}

// TestRetryingNilStaysNil so the harness can wrap unconditionally
// without turning "no model" into a wrapper around nothing.
func TestRetryingNilStaysNil(t *testing.T) {
	if r := Retrying(nil, nil, nil); r != nil {
		t.Errorf("Retrying(nil) = %#v, want nil", r)
	}
	// The typed nil is the trap worth a test: (*RetryingLLM)(nil) handed
	// to a model.LLM parameter is a non-nil interface, so a
	// constructor's `m == nil` guard would wave it through and the
	// dereference would land somewhere with no context.
	if _, err := NewRig(evals.IntentTable{}, nil, Retrying(nil, nil, nil), t.TempDir()); err == nil {
		t.Error("NewRig accepted a nil-wrapping model, want the no-model error")
	}
}

// TestAWaitIsAnnouncedBeforeItIsServed. A 27-second pause nobody
// narrated is indistinguishable from a hung run in a nightly's log.
func TestAWaitIsAnnouncedBeforeItIsServed(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{err: vertex429()},
		{yields: []*adkmodel.LLMResponse{textResponse("ok")}},
	}}
	var announced []string
	r, _ := testRetrying(inner)
	r.onRetry = func(attempt int, wait time.Duration, err error) {
		announced = append(announced, fmt.Sprintf("attempt %d after %s: %v", attempt, wait, err))
	}

	if _, err := drain(t, r); err != nil {
		t.Fatalf("GenerateContent returned %v", err)
	}
	if len(announced) != 1 {
		t.Fatalf("announced %v, want exactly one line", announced)
	}
	if want := "attempt 1 after 3s: failed to call model: "; announced[0][:len(want)] != want {
		t.Errorf("announced %q, want it to start %q", announced[0], want)
	}
}
