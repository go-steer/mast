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

package taskclass_test

import (
	"testing"

	"github.com/go-steer/mast/pkg/taskclass"
)

// TestAgentMode_DesignDocMapping pins the public-class → ADK-mode
// table from docs/orchestration-design.md "Public task classes".
func TestAgentMode_DesignDocMapping(t *testing.T) {
	cases := []struct {
		class string
		want  string
	}{
		{"chat", taskclass.ModeChat},
		{"debug", taskclass.ModeTask},
		{"implement", taskclass.ModeTask},
		{"research", taskclass.ModeTask},
		{"review", taskclass.ModeTask},
		{"orchestrate", taskclass.ModeTask},
	}
	for _, tc := range cases {
		t.Run(tc.class, func(t *testing.T) {
			if got := taskclass.AgentMode(tc.class); got != tc.want {
				t.Errorf("AgentMode(%q) = %q, want %q", tc.class, got, tc.want)
			}
		})
	}
}

func TestAgentMode_UnknownAndEmpty(t *testing.T) {
	if got := taskclass.AgentMode(""); got != "" {
		t.Errorf("AgentMode(\"\") = %q, want \"\"", got)
	}
	// SingleTurn is internal, never a public class
	// (orchestration-design, "SingleTurn is internal").
	for _, class := range []string{"classify", "route", "one-shot", "dispatch", "singleturn"} {
		if got := taskclass.AgentMode(class); got != "" {
			t.Errorf("AgentMode(%q) = %q, want \"\" (SingleTurn is not a public class)", class, got)
		}
	}
}

// TestAgentMode_CoversAllClasses guards the Classes()/AgentMode()
// pairing: adding a seventh class without a mode mapping should fail
// here, not at runtime.
func TestAgentMode_CoversAllClasses(t *testing.T) {
	for _, class := range taskclass.Classes() {
		if taskclass.AgentMode(class) == "" {
			t.Errorf("AgentMode(%q) = \"\"; every public class needs a mode mapping", class)
		}
	}
}

func TestPlannerEnabled_OnlyOrchestrate(t *testing.T) {
	for _, class := range taskclass.Classes() {
		want := class == taskclass.Orchestrate
		if got := taskclass.PlannerEnabled(class); got != want {
			t.Errorf("PlannerEnabled(%q) = %v, want %v", class, got, want)
		}
	}
}

// TestInstruction_Precedence pins the composition contract with
// pkg/agent (modes.go, "Instruction precedence"): Task classes with a
// per-class default return non-empty text (layer 2 — the class
// profile beats the generic mode default), while chat and orchestrate
// return "" so pkg/agent's / pkg/planner's own fallback applies
// (layer 3).
func TestInstruction_Precedence(t *testing.T) {
	for _, class := range []string{"debug", "implement", "research", "review"} {
		if taskclass.Instruction(class) == "" {
			t.Errorf("Instruction(%q) = \"\"; Task classes carry a per-class default", class)
		}
	}
	for _, class := range []string{"chat", "orchestrate", "", "unknown"} {
		if got := taskclass.Instruction(class); got != "" {
			t.Errorf("Instruction(%q) = %q, want \"\" (falls through to the mode/planner default)", class, got)
		}
	}
}

// TestInstruction_DistinctPerClass guards against copy-paste table
// entries: each Task class's default must be its own text.
func TestInstruction_DistinctPerClass(t *testing.T) {
	seen := map[string]string{}
	for _, class := range []string{"debug", "implement", "research", "review"} {
		text := taskclass.Instruction(class)
		if prev, dup := seen[text]; dup {
			t.Errorf("Instruction(%q) duplicates Instruction(%q)", class, prev)
		}
		seen[text] = class
	}
}
