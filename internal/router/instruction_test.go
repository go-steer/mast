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

package router

import (
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/workload"
)

// The coordinator is told to fall back to "_fallback" only when the
// roster has one. The stock gke-triage roster is a generalist named
// "diagnoser" plus change-executor; pointing its coordinator at a
// specialist that does not exist is a delegation that cannot succeed.
func TestDefaultCoordinatorInstructionNamesFallbackOnlyWhenPresent(t *testing.T) {
	routed := defaultCoordinatorInstruction(workload.Bundle{
		Name: "gke-triage-routed", Specialists: []string{"OOMKilled", "_fallback"},
	})
	if !strings.Contains(routed, `"_fallback" specialist`) {
		t.Errorf("a roster with _fallback should be told to fall back to it:\n%s", routed)
	}

	generalist := defaultCoordinatorInstruction(workload.Bundle{
		Name: "gke-triage", Specialists: []string{"diagnoser", "change-executor"},
	})
	if strings.Contains(generalist, "_fallback") {
		t.Errorf("a roster without _fallback should not be told about it:\n%s", generalist)
	}
	if !strings.Contains(generalist, "Consult the tool descriptions") {
		t.Errorf("the instruction lost its routing sentence:\n%s", generalist)
	}
}
