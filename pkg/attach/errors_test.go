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

// Originally derived from go-steer/core-agent@25d8531cf8d1d69459471009a9e7e2e9b0dff1e2

package attach

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/budget"
)

func TestClassifyTurnError_Kinds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		err         error
		wantKind    string
		wantRetry   bool
		wantHintHas string // substring of expected hint, empty means don't check
	}{
		{
			name:        "model_not_found from Vertex 404",
			err:         errors.New(`Error 404, Message: Publisher Model "gemini-x" was not found or your project does not have access to it. Status: NOT_FOUND`),
			wantKind:    TurnErrorModelNotFound,
			wantRetry:   false,
			wantHintHas: "global-only",
		},
		{
			name:      "model_not_found from gRPC name",
			err:       errors.New("rpc error: code = NotFound desc = model not found"),
			wantKind:  TurnErrorModelNotFound,
			wantRetry: false,
		},
		{
			name:        "auth_error from permission denied",
			err:         errors.New("rpc error: code = PermissionDenied desc = caller lacks aiplatform.user"),
			wantKind:    TurnErrorAuth,
			wantRetry:   false,
			wantHintHas: "aiplatform.user",
		},
		{
			name:      "auth_error from 401",
			err:       errors.New("HTTP 401 Unauthorized — invalid credentials"),
			wantKind:  TurnErrorAuth,
			wantRetry: false,
		},
		{
			name:      "rate_limited from 429",
			err:       errors.New("Error 429: Rate exceeded."),
			wantKind:  TurnErrorRateLimited,
			wantRetry: true,
		},
		{
			name:      "rate_limited from gRPC ResourceExhausted",
			err:       errors.New("rpc error: code = ResourceExhausted desc = quota exceeded for tokens-per-minute"),
			wantKind:  TurnErrorRateLimited,
			wantRetry: true,
		},
		{
			name:      "transient_network from gRPC Unavailable",
			err:       errors.New("rpc error: code = Unavailable desc = upstream connect reset"),
			wantKind:  TurnErrorTransientNet,
			wantRetry: true,
		},
		{
			name:      "transient_network from 503",
			err:       errors.New("HTTP 503 Service Unavailable"),
			wantKind:  TurnErrorTransientNet,
			wantRetry: true,
		},
		{
			name:        "config_error from URL parse",
			err:         errors.New(`createAPIURL: error parsing base URL: parse "https://${GOOGLE_CLOUD_LOCATION}-aiplatform.googleapis.com/": invalid character "{" in host name`),
			wantKind:    TurnErrorConfig,
			wantRetry:   false,
			wantHintHas: "GOOGLE_CLOUD_LOCATION",
		},
		{
			name:      "config_error from gRPC InvalidArgument",
			err:       errors.New("rpc error: code = InvalidArgument desc = bad request"),
			wantKind:  TurnErrorConfig,
			wantRetry: false,
		},
		{
			// Pinned deliberately alongside the canceled case below:
			// a turn that ran out of time is worth retrying because
			// nobody asked for it to stop. The two context errors are
			// adjacent in the classifier and must not converge.
			name:      "transient_network from context deadline",
			err:       context.DeadlineExceeded,
			wantKind:  TurnErrorTransientNet,
			wantRetry: true,
		},
		{
			name:      "canceled from context canceled",
			err:       context.Canceled,
			wantKind:  TurnErrorCanceled,
			wantRetry: false,
		},
		{
			// Wrapped, because neither producer hands the bare
			// sentinel over: an interrupt comes back through ADK's
			// runner and the watchdog's halt through the enforcing
			// caller's cancel.
			name:      "canceled through a wrapper",
			err:       fmt.Errorf("run turn: %w", context.Canceled),
			wantKind:  TurnErrorCanceled,
			wantRetry: false,
		},
		{
			name:        "cost_ceiling from a session budget trip",
			err:         errors.New("budget exceeded: $0.0512 > cap $0.0500 (25600 tokens over 4 calls)"),
			wantKind:    TurnErrorCostCeiling,
			wantRetry:   false,
			wantHintHas: "guardrails/reset",
		},
		{
			name:      "cost_ceiling from a specialist budget trip",
			err:       errors.New(`budget exceeded: specialist "log-analyst": 6 model calls (turns) > cap 5`),
			wantKind:  TurnErrorCostCeiling,
			wantRetry: false,
		},
		{
			name:      "unknown for novel errors",
			err:       errors.New("something nobody planned for"),
			wantKind:  TurnErrorUnknown,
			wantRetry: false,
		},
		{
			name:     "unknown for nil error (defensive)",
			err:      nil,
			wantKind: TurnErrorUnknown,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyTurnError(tc.err)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q (full: %+v)", got.Kind, tc.wantKind, got)
			}
			if got.Retryable != tc.wantRetry {
				t.Errorf("Retryable = %v, want %v (kind=%s)", got.Retryable, tc.wantRetry, got.Kind)
			}
			if tc.wantHintHas != "" && !strings.Contains(got.Hint, tc.wantHintHas) {
				t.Errorf("Hint = %q, want substring %q", got.Hint, tc.wantHintHas)
			}
			if got.Kind != TurnErrorUnknown && tc.err != nil && got.Message == "" {
				t.Errorf("Message should be non-empty for classified errors; got %+v", got)
			}
		})
	}
}

// The budget trip is classified by string prefix, because pkg/attach
// must not import pkg/budget — so the sentinel's text is a contract,
// and this is the only thing that notices when it drifts. A miss here
// is silent: the turn still fails, it just reports as "unknown" and
// the operator never learns a reset would fix it.
func TestClassifyTurnError_MatchesTheRealBudgetSentinel(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("%w: $0.0512 > cap $0.0500 (25600 tokens over 4 calls)", budget.ErrExceeded)
	if got := ClassifyTurnError(err); got.Kind != TurnErrorCostCeiling {
		t.Errorf("Kind = %q for %v, want %q", got.Kind, err, TurnErrorCostCeiling)
	}
}

func TestClassifyTurnError_FirstSentenceTrim(t *testing.T) {
	t.Parallel()
	// Multi-line error message should be trimmed to first line.
	err := errors.New("line one says it all\nline two adds stack trace\nline three has another stack frame")
	got := ClassifyTurnError(err)
	if strings.Contains(got.Message, "\n") {
		t.Errorf("Message should be single line; got %q", got.Message)
	}
	if !strings.HasPrefix(got.Message, "line one") {
		t.Errorf("Message should start with first line; got %q", got.Message)
	}
}

func TestClassifyTurnError_LongMessageCapped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1000)
	got := ClassifyTurnError(errors.New(long))
	if len(got.Message) > 240 {
		t.Errorf("Message length = %d, want <= 240 (was capped)", len(got.Message))
	}
	if !strings.HasSuffix(got.Message, "...") {
		t.Errorf("Capped message should end with ellipsis; got %q", got.Message)
	}
}

// TestProtocolV1_5_0_IsAdditive pins what the #206 bump did and did
// not do. The version moved because a client negotiating on it needs
// to know a new turn-error kind exists; nothing else about the wire
// changed, and a future edit that adds an event type or a field under
// cover of "we already bumped for canceled" fails here.
//
// The existing v1.4.0 conformance fixtures deliberately keep their
// literal "1.4.0": they pin the shape that shipped under that version,
// which this change does not touch.
func TestProtocolV1_5_0_IsAdditive(t *testing.T) {
	t.Parallel()

	if protocolVersion != "1.5.0" {
		t.Errorf("protocolVersion = %q, want 1.5.0 — a wire change needs its own entry in the version log above the constant", protocolVersion)
	}

	want := []string{
		EventStatusUpdate,
		EventUsageUpdate,
		EventInbox,
		EventTurnComplete,
		EventTurnError,
		"stream-chunk",
		"tool-call",
		"tool-result",
	}
	if !slices.Equal(supportedEventTypes, want) {
		t.Errorf("supportedEventTypes = %v, want %v — canceled is a new value in an existing enum, not a new frame", supportedEventTypes, want)
	}

	// §2.6 requires a consumer to fall back to unknown on a kind it
	// does not recognize, which is the whole reason this is additive
	// rather than breaking. Pin that canceled is a distinct value and
	// not an alias of something a 1.4.0 client already handles.
	for _, existing := range []string{
		TurnErrorConfig, TurnErrorAuth, TurnErrorModelNotFound,
		TurnErrorRateLimited, TurnErrorTransientNet, TurnErrorCostCeiling,
		TurnErrorUnknown,
	} {
		if TurnErrorCanceled == existing {
			t.Errorf("TurnErrorCanceled collides with %q", existing)
		}
	}
}
