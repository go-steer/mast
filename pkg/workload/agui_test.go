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

package workload

import (
	"strings"
	"testing"
)

// TestAGUIStateProjectionParses pins the field and, more to the point, pins
// that omitting it yields nil rather than something a caller could mistake for
// "project everything" (#98, docs/ag-ui-design.md OQ 7).
func TestAGUIStateProjectionParses(t *testing.T) {
	b, err := loadYAML(t, "agui:\n  expose: true\n  state_projection: [plan, phase]\n"+minimal)
	if err != nil {
		t.Fatalf("state_projection must load: %v", err)
	}
	if got := b.AGUI.StateProjection; len(got) != 2 || got[0] != "plan" || got[1] != "phase" {
		t.Errorf("StateProjection = %v, want [plan phase]", got)
	}

	b, err = loadYAML(t, "agui:\n  expose: true\n"+minimal)
	if err != nil {
		t.Fatalf("an agui section with no state_projection must load: %v", err)
	}
	if got := b.AGUI.StateProjection; got != nil {
		t.Errorf("absent state_projection = %v, want nil — the default publishes nothing", got)
	}
}

// TestAGUIEmitReasoningParses pins the reasoning opt-in and, as with the
// projection above, pins what its ABSENCE means: false, publishing nothing
// (#98, docs/ag-ui-design.md OQ 5). A publication surface whose default came
// from a zero value nobody asserted is one refactor away from inverting.
func TestAGUIEmitReasoningParses(t *testing.T) {
	b, err := loadYAML(t, "agui:\n  expose: true\n  emit_reasoning: true\n"+minimal)
	if err != nil {
		t.Fatalf("emit_reasoning must load: %v", err)
	}
	if !b.AGUI.EmitReasoning {
		t.Error("emit_reasoning: true did not reach the bundle")
	}

	b, err = loadYAML(t, "agui:\n  expose: true\n"+minimal)
	if err != nil {
		t.Fatalf("an agui section with no emit_reasoning must load: %v", err)
	}
	if b.AGUI.EmitReasoning {
		t.Error("absent emit_reasoning = true, want false — the default publishes no reasoning")
	}
}

// TestAGUIDocumentedButUnimplementedKeysAreRefused is the measurement behind
// the 2026-09-14 correction in docs/ag-ui-design.md: that doc's bundle example
// carried `streaming:` and `activity_events:` under `agui:`, neither of which
// is a field. Since #302 an unrecognised key is a load error, so a bundle
// copied from the example did not start the daemon — the example was not
// merely aspirational, it was broken. This test is what keeps the corrected
// example honest: if either key is ever implemented, this fails and whoever
// implements it has to come back and restore the documentation.
func TestAGUIDocumentedButUnimplementedKeysAreRefused(t *testing.T) {
	for _, key := range []string{"streaming: true", "activity_events: true"} {
		_, err := loadYAML(t, "agui:\n  expose: true\n  "+key+"\n"+minimal)
		if err == nil {
			t.Errorf("agui.%s loaded; either implement it or the design doc's example is wrong again", key)
			continue
		}
		if !strings.Contains(err.Error(), "field") {
			t.Errorf("agui.%s: error = %v, want an unknown-field load error", key, err)
		}
	}
}
