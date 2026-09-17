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

package main

import (
	"context"
	"testing"

	"github.com/go-steer/mast/pkg/a2a"
	"github.com/go-steer/mast/pkg/agui"
)

// The daemon's own bearer validators mint ONE principal holding the union of
// every exposed workload's scopes, so a per-workload scope check has no
// reachable failing branch. That is a documented property, not an accident —
// one token has to reach every workload it was configured for — and #389
// records it in docs/ag-ui-design.md § Auth, docs/a2a-design.md, and the docs
// site's "Scopes and the shared token".
//
// This test is a tripwire on those pages. If it starts failing because the
// daemon learned to issue discriminating tokens (#389 option 2), that is the
// good outcome, and the three places above must stop saying the check cannot
// fail before the change ships. Deleting the test is the wrong way to make it
// pass: the claim it pins is in published documentation.
func TestSharedBearerTokenHoldsEveryConfiguredScope(t *testing.T) {
	const token = "shared-secret"
	ctx := context.Background()

	t.Run("agui", func(t *testing.T) {
		t.Setenv("MAST_AGUI_TOKEN", token)
		v, err := aguiValidator(discardLogger(), []agui.ExposedWorkload{
			{WorkloadName: "reports", EndpointPath: "/agui/reports", Scopes: []string{"reports:run"}},
			{WorkloadName: "deploys", EndpointPath: "/agui/deploys", Scopes: []string{"deploy:run"}},
		})
		if err != nil {
			t.Fatalf("aguiValidator: %v", err)
		}
		p, err := v.Validate(ctx, token)
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		for _, scope := range []string{"reports:run", "deploy:run"} {
			if !p.HasScope(scope) {
				t.Errorf("the one AG-UI principal lacks %q, so some exposed workload is unreachable", scope)
			}
		}
		if p.Subject != "mast-agui-static" {
			t.Errorf("subject = %q; the docs say thread ownership separates deployments because this is a constant", p.Subject)
		}
	})

	t.Run("a2a", func(t *testing.T) {
		t.Setenv("MAST_A2A_TOKEN", token)
		v, err := a2aValidator(discardLogger(), []a2a.ExposedSkill{
			{SkillName: "reports", WorkloadName: "reports", Scopes: []string{"reports:run"}},
			{SkillName: "deploys", WorkloadName: "deploys", Scopes: []string{"deploy:run"}},
		})
		if err != nil {
			t.Fatalf("a2aValidator: %v", err)
		}
		p, err := v.Validate(ctx, token)
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		for _, scope := range []string{"reports:run", "deploy:run"} {
			if !p.HasScope(scope) {
				t.Errorf("the one A2A principal lacks %q, so some exposed skill is unreachable", scope)
			}
		}
		if p.Subject != "mast-a2a-static" {
			t.Errorf("subject = %q, want the documented constant", p.Subject)
		}
	})
}
