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

package graph

import (
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

func buildSpec(t *testing.T, name string, mode specialists.Mode) adkagent.Agent {
	t.Helper()
	a, err := specialists.Build(specialists.Spec{
		Name:        name,
		Description: name + " (test)",
		Mode:        mode,
		Instruction: "test instruction",
	}, specialists.BuildOptions{Model: mastagent.NewEchoModel("echo-" + name)})
	if err != nil {
		t.Fatalf("build specialist %q: %v", name, err)
	}
	return a
}

func TestBuildRequiresFallback(t *testing.T) {
	classifier := buildSpec(t, "clf", specialists.ModeSingleTurn)
	_, err := Build(Config{
		Bundle:      workload.Bundle{Name: "w", Specialists: []string{"A"}},
		Classifier:  classifier,
		Specialists: map[string]Specialist{"A": {Agent: buildSpec(t, "A", specialists.ModeTask)}},
	})
	if err == nil || !strings.Contains(err.Error(), FallbackName) {
		t.Fatalf("want fallback-required error, got %v", err)
	}
}

func TestBuildGraphRoot(t *testing.T) {
	classifier := buildSpec(t, "clf", specialists.ModeSingleTurn)
	root, err := Build(Config{
		Bundle:     workload.Bundle{Name: "w", Specialists: []string{"A", FallbackName}},
		Classifier: classifier,
		Specialists: map[string]Specialist{
			"A": {
				Agent:  buildSpec(t, "A", specialists.ModeTask),
				Budget: specialists.Budget{MaxWallclockSeconds: 45},
			},
			FallbackName: {Agent: buildSpec(t, FallbackName, specialists.ModeTask)},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if root.Name() != "w_graph" {
		t.Fatalf("root name = %q, want w_graph", root.Name())
	}
	// classifier + 2 specialists registered for event authorship.
	if got := len(root.SubAgents()); got != 3 {
		t.Fatalf("SubAgents = %d, want 3", got)
	}
}

// nodeConfig is the single seam through which every specialist budget
// reaches its AgentNode's constructor in Build, so asserting its
// mapping is the constructor-level check that max_wallclock_seconds
// becomes NodeConfig.Timeout.
func TestNodeConfigMapsWallclockToTimeout(t *testing.T) {
	cfg := nodeConfig(specialists.Budget{MaxWallclockSeconds: 45})
	if got, want := cfg.Timeout, 45*time.Second; got != want {
		t.Fatalf("Timeout = %v, want %v", got, want)
	}
}

func TestNodeConfigZeroBudgetMeansNoTimeout(t *testing.T) {
	cfg := nodeConfig(specialists.Budget{MaxTurns: 5, MaxCostUSD: 0.5})
	if cfg.Timeout != 0 {
		t.Fatalf("Timeout = %v, want 0 (unbounded at node level)", cfg.Timeout)
	}
}
