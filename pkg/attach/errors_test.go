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
			// The pre-call half (v0.6 W10.2). Same kind and the same
			// remedy as the two above — an operator who has to work out
			// that "refused" and "exceeded" are the same wall is being
			// asked to know mast's internals.
			name:        "cost_ceiling from a refused call",
			err:         errors.New("budget refused the call: would be model call (turn) 3 of a cap of 2"),
			wantKind:    TurnErrorCostCeiling,
			wantRetry:   false,
			wantHintHas: "guardrails/reset",
		},
		{
			name:      "cost_ceiling from a refused specialist call",
			err:       errors.New(`budget refused the call: specialist "log-analyst": $0.0500 already at cap $0.0500 (25000 tokens over 4 calls); any further call exceeds it`),
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
// There are two sentinels since v0.6 W10.2 and both have to land here,
// so both are built from the real thing rather than from a copy of its
// text.
func TestClassifyTurnError_MatchesTheRealBudgetSentinel(t *testing.T) {
	t.Parallel()

	crossed := fmt.Errorf("%w: $0.0512 > cap $0.0500 (25600 tokens over 4 calls)", budget.ErrExceeded)
	refused := fmt.Errorf("%w: would be model call (turn) 3 of a cap of 2", budget.ErrRefused)

	for _, err := range []error{crossed, refused} {
		if got := ClassifyTurnError(err); got.Kind != TurnErrorCostCeiling {
			t.Errorf("Kind = %q for %v, want %q", got.Kind, err, TurnErrorCostCeiling)
		}
	}

	// Same kind, different code. A surface that reported the refusal as
	// BUDGET_EXCEEDED would be claiming spend for a call that was never
	// made, which is the whole distinction W10.2 buys.
	if a, b := ClassifyTurnError(crossed).Code, ClassifyTurnError(refused).Code; a == b {
		t.Errorf("both sentinels classify as code %q; the wire cannot tell a crossed ceiling from a refused call", a)
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

// TestProtocolV1_6_0_IsAdditive pins what the #206 and #208 bumps did
// and did not do. The version moved twice because a client negotiating
// on it needs to know two new turn-error kinds exist; nothing else
// about the wire changed, and a future edit that adds an event type or
// a field under cover of "we already bumped" fails here.
//
// The existing v1.4.0 conformance fixtures deliberately keep their
// literal "1.4.0": they pin the shape that shipped under that version,
// which neither change touches.
func TestProtocolV1_6_0_IsAdditive(t *testing.T) {
	t.Parallel()

	if protocolVersion != "1.6.0" {
		t.Errorf("protocolVersion = %q, want 1.6.0 — a wire change needs its own entry in the version log above the constant", protocolVersion)
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
		t.Errorf("supportedEventTypes = %v, want %v — canceled and watchdog_halt are new values in an existing enum, not new frames", supportedEventTypes, want)
	}

	// §2.6 requires a consumer to fall back to unknown on a kind it
	// does not recognize, which is the whole reason these are additive
	// rather than breaking. Pin that every kind is a distinct value, so
	// a new one can never arrive as an alias of something a 1.4.0
	// client already handles differently.
	kinds := map[string]string{
		"TurnErrorConfig":        TurnErrorConfig,
		"TurnErrorAuth":          TurnErrorAuth,
		"TurnErrorModelNotFound": TurnErrorModelNotFound,
		"TurnErrorRateLimited":   TurnErrorRateLimited,
		"TurnErrorTransientNet":  TurnErrorTransientNet,
		"TurnErrorCostCeiling":   TurnErrorCostCeiling,
		"TurnErrorCanceled":      TurnErrorCanceled,
		"TurnErrorWatchdogHalt":  TurnErrorWatchdogHalt,
		"TurnErrorUnknown":       TurnErrorUnknown,
	}
	seen := make(map[string]string, len(kinds))
	for name, value := range kinds {
		if other, dup := seen[value]; dup {
			t.Errorf("%s and %s are both %q", name, other, value)
		}
		seen[value] = name
	}
}

// declaredKindError is a stand-in for a mast-raised error that knows
// its own kind. The tests below use it rather than *watchdog.TrippedError
// so pkg/attach keeps no dependency on the watchdog, even in test
// builds; that the real type says the same thing is pinned from the
// other side, in pkg/watchdog/enforce_test.go.
type declaredKindError struct {
	kind string
	msg  string
}

func (e *declaredKindError) Error() string         { return e.msg }
func (e *declaredKindError) TurnErrorKind() string { return e.kind }

// A watchdog halt's message is assembled from the looping tool's name
// and up to 200 bytes of its arguments, so before #208 the classifier's
// patterns were matching on text the model wrote: a tool called
// parse_manifest made the halt a config_error and sent the operator to
// check vertex.location. Every message here is a real halt reason shape
// with a decoy in it.
func TestClassifyTurnError_DeclaredKindBeatsTheMessageText(t *testing.T) {
	t.Parallel()

	reasons := []struct {
		name string
		msg  string
	}{
		{
			name: "tool name matches the config patterns",
			msg:  `watchdog halted this session (repeated-tool-call): agent has called parse_manifest with identical args 5 times in a row — possible tool loop. Args: {"path":"/etc/x"}. The session refuses new turns until an operator resets it (POST /sessions/s1/guardrails/reset).`,
		},
		{
			name: "tool args echo a provider not-found",
			msg:  `watchdog halted this session (repeated-tool-call): agent has called k8s_get with identical args 5 times in a row — possible tool loop. Args: {"name":"api","error":"not found"}.`,
		},
		{
			name: "failure streak quotes a permission denied",
			msg:  `watchdog halted this session (tool-failure-streak): three calls in a row returned errors: k8s_get: permission denied on namespace prod.`,
		},
		{
			name: "nothing in the text to match on",
			msg:  `watchdog halted this session (alternating-tool-cycle): agent has repeated the same 2-call sequence (list_pods -> get_pod) 3 times with identical args.`,
		},
	}

	for _, r := range reasons {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyTurnError(&declaredKindError{kind: TurnErrorWatchdogHalt, msg: r.msg})
			if got.Kind != TurnErrorWatchdogHalt {
				t.Errorf("Kind = %q, want %q — the error said what it was and the patterns overrode it", got.Kind, TurnErrorWatchdogHalt)
			}
			if got.Retryable {
				t.Error("Retryable = true — a retry re-drives the loop the halt exists to break")
			}
			if !strings.Contains(got.Hint, "guardrails/reset") {
				t.Errorf("Hint = %q, want the reset endpoint — the halt reason carries it too, past where firstSentence cuts", got.Hint)
			}
		})
	}
}

// The kind travels through wrapping, because a caller adding turn
// context to a halt must not silently un-classify it.
func TestClassifyTurnError_DeclaredKindSurvivesWrapping(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("turn 3: %w", &declaredKindError{kind: TurnErrorWatchdogHalt, msg: "watchdog halted this session (repeated-tool-call): parse loop"})
	if got := ClassifyTurnError(err); got.Kind != TurnErrorWatchdogHalt {
		t.Errorf("Kind = %q, want %q", got.Kind, TurnErrorWatchdogHalt)
	}
}

// A kind this package does not ship falls back to the patterns rather
// than reaching the wire. §2.6's unknown fallback is a promise about
// values mast ships, not cover for a typo in a raiser.
func TestClassifyTurnError_UnknownDeclaredKindFallsBack(t *testing.T) {
	t.Parallel()
	err := &declaredKindError{kind: "watchdog-halt", msg: "rpc error: code = PermissionDenied desc = caller lacks aiplatform.user"}
	got := ClassifyTurnError(err)
	if got.Kind != TurnErrorAuth {
		t.Errorf("Kind = %q, want %q — an unrecognized declared kind should not shadow the heuristics", got.Kind, TurnErrorAuth)
	}
}
