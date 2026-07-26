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

// External-package tests: everything here goes through the public
// import path, exactly as a library consumer would. The first test is
// fork-design exit criterion 8 verbatim: "mast.RunWorkload(ctx, ...)
// succeeds from a Go test with programmatic bundle registration".
package mast_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/session"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// triageBundle is a programmatic workload: no YAML, no .tmpl files, no
// .agents/ discovery — plain pkg/workload + pkg/specialists values.
func triageBundle(hitl bool) (workload.Bundle, []specialists.Spec) {
	bundle := workload.Bundle{
		Name:        "triage",
		Description: "Programmatic triage workload for the library API test.",
		Specialists: []string{"classify", "ImagePullBackOff", "_fallback"},
		HITL:        workload.HITL{RequireApproval: hitl},
	}
	specs := []specialists.Spec{
		{
			Name:        "classify",
			Description: "Classifies an incident envelope into a failure mode.",
			Mode:        specialists.ModeSingleTurn,
			Instruction: "Reply with the single failure-mode keyword for the incident envelope.",
		},
		{
			Name:        "ImagePullBackOff",
			Description: "Diagnoses image-pull failures.",
			Mode:        specialists.ModeTask,
			Instruction: "Diagnose the image pull failure and finish with a digest.",
		},
		{
			Name:        "_fallback",
			Description: "Handles failure modes with no dedicated specialist.",
			Mode:        specialists.ModeTask,
			Instruction: "Diagnose the incident generically and finish with a digest.",
		},
	}
	return bundle, specs
}

const injectInput = `INJECT {"reason":"ImagePullBackOff","namespace":"default","name":"web-1"}`

// TestRunWorkloadProgrammaticBundle is fork-design exit criterion 8:
// RunWorkload succeeds from a Go test with a programmatically
// registered bundle + echo-model specialists, returning the expected
// result and usage. The roster has a SingleTurn classifier and a
// _fallback, so dispatch is the workflow graph: classifier call +
// routed specialist call = two model calls on the meter.
func TestRunWorkloadProgrammaticBundle(t *testing.T) {
	bundle, specs := triageBundle(false)
	res, err := mast.RunWorkload(context.Background(), mast.Config{ModelName: "echo"},
		bundle, specs, injectInput)
	if err != nil {
		t.Fatalf("RunWorkload: %v", err)
	}
	// The routed Task specialist terminates via finish_task; its result
	// is the turn's final node output. (The echo model's digest does
	// not include the reason here: graph node inputs are re-marshalled
	// between nodes, which escapes the envelope's quotes past the echo
	// fake's reason regex — identical behavior to cmd/mast's graph
	// dispatch, whose demo asserts routing by event authorship.)
	if !strings.Contains(res.Output, "[echo triage] diagnosed from envelope") {
		t.Errorf("Output = %q, want the echo specialist's finish_task digest", res.Output)
	}
	if res.SessionID == "" {
		t.Error("SessionID is empty")
	}
	if res.Usage.ModelCalls != 2 {
		t.Errorf("Usage.ModelCalls = %d, want 2 (classifier + routed specialist)", res.Usage.ModelCalls)
	}
	if res.Usage.Tokens <= 0 {
		t.Errorf("Usage.Tokens = %d, want > 0", res.Usage.Tokens)
	}
	if res.Usage.CostUSD <= 0 {
		t.Errorf("Usage.CostUSD = %v, want > 0 (echo pricing is non-zero by design)", res.Usage.CostUSD)
	}
}

// TestRunWorkloadCoordinatorShape: without a SingleTurn classifier the
// auto dispatch falls back to the SubAgents coordinator — one
// coordinator model call under the echo model.
func TestRunWorkloadCoordinatorShape(t *testing.T) {
	bundle := workload.Bundle{
		Name:        "triage_coordinator",
		Specialists: []string{"ImagePullBackOff"},
	}
	specs := []specialists.Spec{{
		Name:        "ImagePullBackOff",
		Description: "Diagnoses image-pull failures.",
		Mode:        specialists.ModeTask,
		Instruction: "Diagnose the image pull failure and finish with a digest.",
	}}
	res, err := mast.RunWorkload(context.Background(), mast.Config{ModelName: "echo"},
		bundle, specs, injectInput)
	if err != nil {
		t.Fatalf("RunWorkload: %v", err)
	}
	if res.Output == "" {
		t.Error("Output is empty")
	}
	if res.Usage.ModelCalls != 1 {
		t.Errorf("Usage.ModelCalls = %d, want 1 (coordinator only)", res.Usage.ModelCalls)
	}
}

// TestRunWorkloadBudgetOverride: Config.Budget overrides the bundle's
// (absent) ceilings; a one-turn cap trips on the graph's second model
// call and surfaces budget.ErrExceeded.
func TestRunWorkloadBudgetOverride(t *testing.T) {
	bundle, specs := triageBundle(false)
	_, err := mast.RunWorkload(context.Background(),
		mast.Config{ModelName: "echo", Budget: &budget.Limits{MaxTurns: 1}},
		bundle, specs, injectInput)
	if !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("RunWorkload with MaxTurns=1: err = %v, want budget.ErrExceeded", err)
	}
}

// TestRun is the single-agent convenience path: instruction + input,
// one model call, echo output.
func TestRun(t *testing.T) {
	res, err := mast.Run(context.Background(), mast.Config{ModelName: "echo"},
		"Acknowledge the message briefly.", "hello from the host service")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "acknowledged") {
		t.Errorf("Output = %q, want the echo acknowledgement", res.Output)
	}
	if res.Usage.ModelCalls != 1 {
		t.Errorf("Usage.ModelCalls = %d, want 1", res.Usage.ModelCalls)
	}
}

// TestRunRequiresModel: the zero Config is rejected, not defaulted.
func TestRunRequiresModel(t *testing.T) {
	if _, err := mast.Run(context.Background(), mast.Config{}, "x", "y"); err == nil {
		t.Fatal("Run with zero Config: want error, got nil")
	}
}

// TestListSessionsRequiresService: a nil session service can never
// hold listable sessions; the call fails loudly instead of returning
// an empty list.
func TestListSessionsRequiresService(t *testing.T) {
	if _, err := mast.ListSessions(context.Background(), mast.Config{ModelName: "echo"}); err == nil {
		t.Fatal("ListSessions without Config.Sessions: want error, got nil")
	}
}

// TestHITLPauseListResume drives the full durable-HITL loop through
// the library surface on one shared session service: RunWorkload
// parks on the approval interrupt, ListSessions reports the paused
// session with its pending interrupt ID, and ResumeSession feeds the
// operator verdict back over the spike-2 resume wire shape.
func TestHITLPauseListResume(t *testing.T) {
	ctx := context.Background()
	cfg := mast.Config{
		ModelName: "echo",
		Sessions:  adksession.InMemoryService(),
	}
	bundle, specs := triageBundle(true)

	res, err := mast.RunWorkload(ctx, cfg, bundle, specs, injectInput)
	if err != nil {
		t.Fatalf("RunWorkload: %v", err)
	}

	sessions, err := mast.ListSessions(ctx, cfg)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions returned %d sessions, want 1: %+v", len(sessions), sessions)
	}
	got := sessions[0]
	if got.ID != res.SessionID {
		t.Errorf("session ID = %q, want %q", got.ID, res.SessionID)
	}
	if got.State != session.StatePaused {
		t.Fatalf("session state = %q, want %q", got.State, session.StatePaused)
	}
	if len(got.PendingInterruptIDs) != 1 || got.PendingInterruptIDs[0] != "approve-ImagePullBackOff" {
		t.Fatalf("pending interrupts = %v, want [approve-ImagePullBackOff]", got.PendingInterruptIDs)
	}

	resumed, err := mast.ResumeSession(ctx, cfg, bundle, specs,
		res.SessionID, got.PendingInterruptIDs[0], map[string]any{"approved": true})
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if !strings.Contains(resumed.Output, "approval") || !strings.Contains(resumed.Output, "true") {
		t.Errorf("resumed Output = %q, want the approval verdict folded into the node result", resumed.Output)
	}

	after, err := mast.ListSessions(ctx, cfg)
	if err != nil {
		t.Fatalf("ListSessions after resume: %v", err)
	}
	if after[0].State != session.StateIdle {
		t.Errorf("post-resume state = %q, want %q", after[0].State, session.StateIdle)
	}
}
