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
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
)

// spend fakes one model call authored by the named agent.
func spend(author string, tokens int32) *session.Event {
	return &session.Event{
		Author: author,
		LLMResponse: model.LLMResponse{
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				TotalTokenCount: tokens,
			},
		},
	}
}

// The daemon's meter is sized from the roster buildRoot loaded, not
// from the bundle alone — so a specialist's declared max_cost_usd is
// enforced on the served path and not just in pkg/budget's unit tests.
// The shipped gke-triage workload is the fixture precisely because it
// declares no session cost or turn ceiling of its own: the only thing
// that can stop this run is the specialist's own $0.25.
func TestMeterPoolEnforcesSpecialistCeilings(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "workloads", "gke-triage")
	_, bundle, specs, err := buildRoot(context.Background(), discardLogger(),
		mastagent.NewEchoModel("echo"), "", "echo", dir, "coordinator", nil)
	if err != nil {
		t.Fatalf("buildRoot: %v", err)
	}
	if len(specs) == 0 {
		t.Fatal("buildRoot returned no roster; the meter pool has nothing to scope")
	}
	if bundle.Budget.MaxCostUSD != 0 || bundle.Budget.MaxTurns != 0 {
		t.Fatalf("fixture is not discriminating: the workload declares its own ceilings (%+v)", bundle.Budget)
	}

	pool := newMeterPool(bundle, specs, "echo")

	// 10k tokens at echo's $0.05/1K is $0.50 — twice OOMKilled's
	// declared $0.25, and against an unbounded session.
	err = pool.meter("incident-abc").Observe(spend("OOMKilled", 10_000))
	if !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("OOMKilled overspent its declared cap; meter said %v", err)
	}
	if !strings.Contains(err.Error(), "OOMKilled") {
		t.Errorf("error should name the specialist whose ceiling stopped the run: %v", err)
	}

	// The coordinator is not in the roster and declares no budget, so
	// the same spend under an unbounded workload is free.
	const coordinator = "triage_coordinator"
	for _, s := range specs {
		if s.Name == coordinator {
			t.Fatalf("fixture is not discriminating: %q is a roster specialist", coordinator)
		}
	}
	if err := pool.meter("incident-abc").Observe(spend(coordinator, 10_000)); err != nil {
		t.Errorf("unscoped coordinator spend under an unbounded workload: %v", err)
	}

	// Scopes are per session: the next incident starts at zero.
	if err := pool.meter("incident-xyz").Observe(spend("OOMKilled", 1_000)); err != nil {
		t.Errorf("a fresh session inherited the previous one's spend: %v", err)
	}
}
