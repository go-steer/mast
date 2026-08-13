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

package workload_test

import (
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/workload"
)

// TestHITL_DefaultIsRequireApproval pins the normative default from
// docs/orchestration-design.md's field reference. A bundle that says
// nothing about mutations gets the safe answer, and the default lives
// on the bundle so that adding the key later cannot quietly loosen it.
func TestHITL_DefaultIsRequireApproval(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := b.HITL.EffectiveOnMutation(), workload.OnMutationRequireApproval; got != want {
		t.Errorf("EffectiveOnMutation() = %q, want %q — an unconfigured bundle must not apply mutations unattended", got, want)
	}
}

func TestHITL_OnMutationRoundTrips(t *testing.T) {
	for _, want := range []workload.OnMutation{
		workload.OnMutationRequireApproval,
		workload.OnMutationApply,
		workload.OnMutationDryRun,
	} {
		path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nhitl:\n  on_mutation: "+string(want)+"\n")
		b, err := workload.Load(path)
		if err != nil {
			t.Fatalf("Load(on_mutation: %s): %v", want, err)
		}
		if got := b.HITL.EffectiveOnMutation(); got != want {
			t.Errorf("EffectiveOnMutation() = %q, want %q", got, want)
		}
	}
}

// TestHITL_UnknownOnMutationIsRefusedAtLoad is the whole reason the enum
// is validated at load: a typo must not degrade to a default. Which
// default it degraded to would decide whether a cluster gets changed.
func TestHITL_UnknownOnMutationIsRefusedAtLoad(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nhitl:\n  on_mutation: aply\n")
	_, err := workload.Load(path)
	if err == nil {
		t.Fatalf("Load accepted on_mutation: aply")
	}
	if !strings.Contains(err.Error(), "on_mutation") {
		t.Errorf("error = %v, want it to name the offending key", err)
	}
}

// TestHITL_PolicyAliasFolds accepts the spelling the design doc uses.
func TestHITL_PolicyAliasFolds(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nhitl_policy:\n  on_mutation: dry_run\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := b.HITL.EffectiveOnMutation(), workload.OnMutationDryRun; got != want {
		t.Errorf("EffectiveOnMutation() = %q, want %q — hitl_policy: is the documented spelling and must reach the same field", got, want)
	}
	if b.HITLPolicy != (workload.HITL{}) {
		t.Errorf("HITLPolicy = %+v after load, want it emptied so nothing downstream reads two places", b.HITLPolicy)
	}
}

// TestHITL_BothSpellingsIsAnError: with two blocks there is no reading
// of the author's intent that is better than asking them.
func TestHITL_BothSpellingsIsAnError(t *testing.T) {
	path := writeBundle(t, "b.yaml",
		"name: x\nspecialists: [a]\nhitl:\n  on_mutation: apply\nhitl_policy:\n  on_mutation: require_approval\n")
	_, err := workload.Load(path)
	if err == nil {
		t.Fatalf("Load accepted both hitl: and hitl_policy:")
	}
	if !strings.Contains(err.Error(), "hitl_policy") {
		t.Errorf("error = %v, want it to name both spellings", err)
	}
}

// TestHITL_RequireApprovalIsSeparateFromOnMutation: require_approval
// gates the RESULT of a run, on_mutation gates each mutating call. They
// are configured independently and one must not imply the other.
func TestHITL_RequireApprovalIsSeparateFromOnMutation(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\nhitl:\n  require_approval: true\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !b.HITL.RequireApproval {
		t.Error("RequireApproval = false, want true")
	}
	if got, want := b.HITL.EffectiveOnMutation(), workload.OnMutationRequireApproval; got != want {
		t.Errorf("EffectiveOnMutation() = %q, want %q", got, want)
	}

	path = writeBundle(t, "c.yaml", "name: x\nspecialists: [a]\nhitl:\n  on_mutation: apply\n")
	b, err = workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if b.HITL.RequireApproval {
		t.Error("RequireApproval = true, want false — on_mutation must not imply the result gate")
	}
}
