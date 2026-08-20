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

// The one place this package's contract with the wire layer is checked.
//
// TrippedError implements attach.SelfClassifyingError by returning a
// bare string, because pkg/watchdog is stdlib-only and importing
// pkg/attach — and with it auth, eventlog and permissions — to spell
// one constant is not a trade worth making for a leaf guardrail
// package. A bare string is a weaker contract than a type, so the
// obligation lands here: this file is a test-only import of pkg/attach,
// and it fails if the two sides ever stop agreeing.

package watchdog_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/watchdog"
)

var _ attach.SelfClassifyingError = (*watchdog.TrippedError)(nil)

func TestTrippedErrorDeclaresTheAttachKind(t *testing.T) {
	t.Parallel()
	got := (&watchdog.TrippedError{}).TurnErrorKind()
	if got != attach.TurnErrorWatchdogHalt {
		t.Errorf("TurnErrorKind() = %q, want attach.TurnErrorWatchdogHalt (%q) — a halt that names a kind pkg/attach does not ship falls back to substring-scanning its own reason, which is the #208 defect",
			got, attach.TurnErrorWatchdogHalt)
	}
}

// End to end over the real halt: the reason a real Enforcer writes,
// through the real classifier, to the frame an operator reads.
//
// The reason is built from the offending tool's name and its
// model-supplied arguments, and this one is shaped to trip three of the
// classifier's patterns at once — "parse", "not found" and "permission
// denied". Before #208 the first of those won and the operator was told
// to go check model.vertex.location during a runaway-loop incident.
func TestAHaltIsNotReportedAsAProviderProblem(t *testing.T) {
	t.Parallel()

	e := watchdog.NewEnforcer(watchdog.ModeEnforce, "The session refuses new turns until an operator resets it (POST /sessions/s1/guardrails/reset).")
	if !e.Observe(watchdog.Alert{
		Signal:   "repeated-tool-call",
		Severity: watchdog.SeverityCritical,
		Reason:   `agent has called parse_manifest with identical args 5 times in a row — possible tool loop. Args: {"ns":"prod","last_error":"not found: permission denied"}.`,
	}) {
		t.Fatal("a Critical alert under enforce must halt")
	}

	// Preflight's refusal, wrapped the way a caller adding turn context
	// would wrap it.
	err := fmt.Errorf("turn %q: %w", "t4", e.Preflight())

	got := attach.ClassifyTurnError(err)
	if got.Kind != attach.TurnErrorWatchdogHalt {
		t.Errorf("Kind = %q, want %q — the halt was classified by scanning text the model wrote", got.Kind, attach.TurnErrorWatchdogHalt)
	}
	if got.Retryable {
		t.Error("Retryable = true — the refusal stands until an operator resets, so every retry fails the same way")
	}
	if !strings.Contains(got.Hint, "guardrails/reset") {
		t.Errorf("Hint = %q, want the reset endpoint", got.Hint)
	}
	if strings.Contains(got.Hint, "vertex.location") {
		t.Errorf("Hint = %q — sending an operator to the provider config during a tool loop is the #208 defect", got.Hint)
	}
	if !strings.Contains(got.Message, "repeated-tool-call") {
		t.Errorf("Message = %q, want the halting signal named", got.Message)
	}
}
