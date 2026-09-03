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
	"regexp"
	"strings"
)

// SelfClassifyingError is implemented by an error mast raised itself,
// which already knows which turn-error kind it is and should not have
// that read back out of its own prose.
//
// It carries the kind and nothing else on purpose. Code, hint and
// retryability are wire text, and wire text belongs here: a raiser
// would have to import this package to name a TurnError, and the two
// raisers that matter are leaf packages that shouldn't. pkg/watchdog is
// stdlib-only; pulling in pkg/attach — and with it auth, eventlog and
// permissions — to spell one constant is the same coupling
// ClassifyTurnError already refused in the other direction for
// pkg/budget (#135). A bare string is a weaker contract than a type, so
// the implementations pin theirs against these constants in their own
// tests, and classifyDeclaredKind ignores a value it doesn't recognize.
//
// Kinds not covered here stay on the patterns below, which is where
// pkg/budget's ErrExceeded remains: it is a wrapped sentinel rather
// than a named type, so there is nothing to hang a method on.
type SelfClassifyingError interface {
	error

	// TurnErrorKind returns one of the TurnError* kind constants in
	// this package.
	TurnErrorKind() string
}

// classifyDeclaredKind renders the TurnError for a kind an error
// declared about itself. Reports false for a kind this package does not
// ship, leaving the caller to fall back on the heuristics.
func classifyDeclaredKind(kind string, err error) (TurnError, bool) {
	// One kind today; an if rather than a switch only because gocritic
	// says so about a switch with one case.
	if kind == TurnErrorWatchdogHalt {
		return TurnError{
			Kind:      TurnErrorWatchdogHalt,
			Code:      "WATCHDOG_HALT",
			Message:   firstSentence(err.Error()),
			Retryable: false,
			// The halt reason carries this remedy too, but at the end
			// of a sentence long enough that firstSentence cuts it off
			// — the tool name and arguments come first. Repeat it
			// where it survives.
			Hint: "The watchdog halted this session and every further turn refuses the same way. Fix what looped, then clear it with POST /sessions/{id}/guardrails/reset.",
		}, true
	}
	return TurnError{}, false
}

// ClassifyTurnError maps a raw error from a turn to a TurnError
// payload conforming to the SSE event-stream protocol's kind enum
// (spec section 2.6). Most of what reaches it comes off the model
// call, but not all of it — the turn's own context arrives here too,
// so "the model failed" is not a safe reading of every result.
//
// Classification is string-based rather than type-based because the
// genai / ADK / Vertex / Anthropic clients each wrap upstream
// errors differently and changing wrapper layers would silently
// regress a type-switch. String matching against the canonical
// status names (gRPC code names, HTTP status numbers, well-known
// substrings) survives wrapper churn.
//
// That reasoning covers errors that arrive from outside. It does not
// cover the ones mast raises itself, which know what they are and
// should not be guessed at: those implement SelfClassifyingError and
// are honored ahead of every pattern below.
//
// The hint field is populated with the most actionable next step
// when one is obvious — operators reading these in a chat-bubble
// shouldn't need to leave the TUI to know what to try.
func ClassifyTurnError(err error) TurnError {
	if err == nil {
		return TurnError{Kind: TurnErrorUnknown, Message: "nil error", Retryable: false}
	}

	// An error that names its own kind wins over anything the patterns
	// below could find in its text — and the watchdog case shows why
	// that ordering is not merely tidy. A halt's message is built from
	// the looping tool's name and its model-supplied arguments, so a
	// substring scan lets the agent pick its own label: a tool called
	// parse_manifest classified as config_error, arguments echoing
	// "not found" as model_not_found (#208).
	var sc SelfClassifyingError
	if errors.As(err, &sc) {
		if te, ok := classifyDeclaredKind(sc.TurnErrorKind(), err); ok {
			return te
		}
		// An unrecognized kind falls through rather than reaching the
		// wire: §2.6's `unknown` fallback is a contract with clients
		// about values mast ships, not a place to put a typo.
	}

	// Context errors come through unwrapped from cancellation /
	// deadline plumbing; check before string matching since
	// errors.Is is more reliable than substring scan for these.
	if errors.Is(err, context.DeadlineExceeded) {
		return TurnError{
			Kind:      TurnErrorTransientNet,
			Code:      "DEADLINE_EXCEEDED",
			Message:   "model call timed out",
			Retryable: true,
		}
	}
	// A cancel is a deliberate stop, never a failure to retry. In an
	// unattended daemon it has two live producers and both mean the
	// opposite of "the network hiccuped": an operator interrupt over
	// attach, and the watchdog halting a runaway loop in flight under
	// --watchdog=enforce. Telling a client that either one is a
	// retryable transient network fault invites it to re-drive exactly
	// the turn somebody just stopped.
	//
	// Note the asymmetry with the deadline above, which stays
	// retryable: nobody asked for that one.
	if errors.Is(err, context.Canceled) {
		return TurnError{
			Kind:      TurnErrorCanceled,
			Code:      "CANCELED",
			Message:   "turn canceled",
			Retryable: false,
		}
	}

	msg := err.Error()
	lower := strings.ToLower(msg)
	code := extractStatusCode(msg)

	switch {
	// Budget — mast's own meter stopped the turn, not the provider.
	// First in the switch because it is the only case whose message is
	// authored here: pkg/budget's ErrExceeded wraps a detail string
	// ("$0.0612 > cap $0.0500 (30600 tokens over 4 calls)") that
	// contains numbers the later HTTP-status and "parse" patterns
	// could otherwise pick at. Matched by string like everything else
	// in this classifier — a pkg/attach → pkg/budget import to run
	// errors.Is would couple the wire layer to the meter for one
	// prefix (#135).
	case strings.HasPrefix(lower, "budget exceeded"):
		return TurnError{
			Kind:      TurnErrorCostCeiling,
			Code:      "BUDGET_EXCEEDED",
			Message:   firstSentence(msg),
			Retryable: false,
			Hint:      "The session is past a budget ceiling and every further turn will stop the same way. Raise it with POST /sessions/{id}/guardrails/reset (additional_budget_usd / additional_tokens / additional_turns).",
		}

	// The pre-call half of the same ceiling (v0.6 W10.2): the meter
	// refused a call rather than reporting one that crossed. Same kind
	// and the same remedy — from an operator's side this is one event,
	// "the budget stopped the session" — but a distinct code, because
	// the two differ in whether any money was spent and a surface that
	// reported "exceeded" here would be claiming spend that did not
	// happen. Kept adjacent to the case above for the same reason it is
	// first: the reason string carries figures the HTTP-status patterns
	// below would otherwise pick at.
	case strings.HasPrefix(lower, "budget refused"):
		return TurnError{
			Kind:      TurnErrorCostCeiling,
			Code:      "BUDGET_REFUSED",
			Message:   firstSentence(msg),
			Retryable: false,
			Hint:      "A budget ceiling could not be respected by making the next model call, so it was not made — nothing was spent, and every further turn stops the same way. Raise it with POST /sessions/{id}/guardrails/reset (additional_budget_usd / additional_tokens / additional_turns).",
		}

	// NotFound — typically a model name / location mismatch
	// (e.g. global-only model requested at a regional endpoint).
	case containsAny(lower, "not_found", "not found") || code == "404":
		return TurnError{
			Kind:      TurnErrorModelNotFound,
			Code:      coalesce(code, "NOT_FOUND"),
			Message:   firstSentence(msg),
			Retryable: false,
			Hint:      "Check the model name and vertex.location (some models are global-only).",
		}

	// Auth — IAM / credentials / OAuth failures. Match both the
	// underscored ("permission_denied") and CamelCase-as-one-word
	// ("permissiondenied") forms — gRPC error strings emit the
	// latter after the lowercase pass.
	case containsAny(lower, "permission_denied", "permissiondenied", "unauthenticated",
		"permission denied", "unauthorized", "invalid credentials",
		"could not find default credentials", "forbidden") || code == "401" || code == "403":
		return TurnError{
			Kind:      TurnErrorAuth,
			Code:      coalesce(code, "PERMISSION_DENIED"),
			Message:   firstSentence(msg),
			Retryable: false,
			Hint:      "Verify the runtime service account has roles/aiplatform.user (or the provider-equivalent role) and that GOOGLE_APPLICATION_CREDENTIALS / ADC is set.",
		}

	// Rate-limit / quota — retryable with backoff.
	case containsAny(lower, "resource_exhausted", "resourceexhausted", "rate exceeded",
		"rate limit", "quota exceeded", "too many requests") || code == "429":
		return TurnError{
			Kind:      TurnErrorRateLimited,
			Code:      coalesce(code, "RESOURCE_EXHAUSTED"),
			Message:   firstSentence(msg),
			Retryable: true,
		}

	// Transient network — usually retryable.
	case containsAny(lower, "deadline_exceeded", "deadlineexceeded", "unavailable",
		"connection refused", "connection reset", "no such host",
		"temporary failure", "i/o timeout") ||
		code == "503" || code == "504" || code == "502":
		return TurnError{
			Kind:      TurnErrorTransientNet,
			Code:      coalesce(code, "UNAVAILABLE"),
			Message:   firstSentence(msg),
			Retryable: true,
		}

	// Config — URL parse failures, missing required values, malformed
	// inputs caught client-side before any RPC fires. These don't
	// retry on their own.
	case containsAny(lower, "invalid_argument", "invalidargument", "failed_precondition",
		"failedprecondition", "invalid character", "parse", "createapiurl") || code == "400":
		return TurnError{
			Kind:      TurnErrorConfig,
			Code:      coalesce(code, "INVALID_ARGUMENT"),
			Message:   firstSentence(msg),
			Retryable: false,
			Hint:      "Check the model provider config (model.vertex.location, model.name, GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_LOCATION).",
		}
	}

	// Unknown — preserve the message so operators can still see what
	// happened, even though we couldn't categorize it.
	return TurnError{
		Kind:      TurnErrorUnknown,
		Code:      code,
		Message:   firstSentence(msg),
		Retryable: false,
	}
}

// containsAny returns true if s contains any of needles.
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// coalesce returns the first non-empty argument.
func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// statusCodeRE matches HTTP status numbers in common error message
// formats: "Error 404,", "status: 404", "404 Not Found", etc. The
// extractStatusCode below uses this to pull a code even when the
// upstream library doesn't expose a structured status.
var statusCodeRE = regexp.MustCompile(`\b(?:Error |status[: ]+|code[: ]+|HTTP )(\d{3})\b`)

// extractStatusCode pulls an HTTP-style status number out of an
// error message if one is present. Returns "" if not found.
func extractStatusCode(msg string) string {
	m := statusCodeRE.FindStringSubmatch(msg)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// firstSentence trims the message to a single user-readable line,
// capped at a length that fits comfortably in a chat-bubble render.
// Multi-line errors from upstream APIs often include a stack trace
// or URL dump; surfacing the whole block in the TUI's chat window
// crowds out everything else.
func firstSentence(msg string) string {
	if i := strings.IndexAny(msg, "\n"); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(msg)
	const cap = 240
	if len(msg) > cap {
		msg = msg[:cap-3] + "..."
	}
	return msg
}
