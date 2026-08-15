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
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

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

// TestGraphRunSurfacesExternalCancellation pins the ADK substrate
// guarantee that a graph run whose invocation context dies from outside
// the scheduler reports the cancellation rather than success.
//
// This is a v2.2.0 fix, not a long-standing property: under v2.1.0 the
// scheduler only reacted to cancellation in its doneChan select arm,
// and a ready doneChan does not have to win against a queued node
// completion — so a run could drain and return cleanly after being
// cancelled. cancelAll never touches parentCtx, so nothing else caught
// it. Verified against both versions when the v2.2.0 bump landed: this
// test fails on v2.1.0 and passes on v2.2.0.
//
// mast cancels in-flight turns from outside the graph (session eviction
// via the attach registry's cancelOnEvict, the dispatch deadline, the
// daemon-level abort in pkg/transcript), and every one of those paths
// runs through this scheduler — a cancelled run that reports success is
// a cancelled run the durable-execution machinery records as finished.
// If an upgrade ever makes this test fail again, treat it as a
// durability regression, not a flaky test.
func TestGraphRunSurfacesExternalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	// The node reports nothing of its own on the way out: it observes
	// the dying context and returns cleanly. That is the shape that
	// used to be reported as success — a node with its own error was
	// always surfaced.
	blocker := workflow.NewFunctionNode("blocker",
		func(nc adkagent.Context, _ any) (any, error) {
			close(entered)
			<-nc.Done()
			return nil, nil
		}, workflow.NodeConfig{})

	root, err := workflowagent.New(workflowagent.Config{
		Name:  "cancel_probe",
		Edges: workflow.Chain(workflow.Start, blocker),
	})
	if err != nil {
		t.Fatalf("workflowagent.New: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           "graph-test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	go func() {
		<-entered
		cancel()
	}()

	var runErr error
	for _, err := range r.Run(ctx, "op", "s1", genai.NewContentFromText("go", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			runErr = err
		}
	}
	if runErr == nil {
		t.Fatal("cancelled graph run reported success; the invocation context died mid-node and nothing surfaced it")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("run error = %v, want it to wrap context.Canceled", runErr)
	}
}

// TestRouteKeyStickyOnResume pins the resume asymmetry. A confirmation
// resume re-enters the graph at START, so the classifier runs again on
// the operator's answer ("yes"), which classifies as nothing. Believing
// that reply sends the answer to _fallback while the specialist that
// parked waits on a turn it never sees — the failure the v0.4 handoff
// UAT hit as `no function call event found for function responses ids`.
func TestRouteKeyStickyOnResume(t *testing.T) {
	known := map[string]string{"oomkilled": "OOMKilled", "_fallback": FallbackName}
	for _, tc := range []struct {
		name  string
		reply any
		prior string
		want  string
	}{
		{"a named specialist wins", "OOMKilled", FallbackName, "OOMKilled"},
		{"case and a trailing period are tolerated", "oomkilled.", "", "OOMKilled"},
		{"an unrecognized reply keeps the recorded route", "yes", "OOMKilled", "OOMKilled"},
		{"a blank reply keeps the recorded route", "  ", "OOMKilled", "OOMKilled"},
		// Nothing recorded yet: the first pass must still be free to
		// reach the Default edge, or an incident with no specialist
		// stops routing to _fallback.
		{"a first pass with nothing recorded routes as-is", "NoSuchReason", "", "NoSuchReason"},
		{"a blank prior is not a route", "NoSuchReason", "   ", "NoSuchReason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeKey(tc.reply, known, tc.prior); got != tc.want {
				t.Errorf("routeKey(%q, prior=%q) = %q, want %q", tc.reply, tc.prior, got, tc.want)
			}
		})
	}
}
