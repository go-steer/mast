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
	"testing"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/a2a"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// newA2ABackend builds an a2aBackend over an in-memory transcript store
// with the given sessions pre-created (the runner creates sessions on
// first turn; tests seed them up front, mirroring trackerFixture).
func newA2ABackend(t *testing.T, sessionIDs ...string) (*a2aBackend, *transcript.Store) {
	t.Helper()
	svc := adksession.InMemoryService()
	for _, sid := range sessionIDs {
		if _, err := svc.Create(context.Background(), &adksession.CreateRequest{
			AppName:   appName,
			UserID:    defaultUserID,
			SessionID: sid,
		}); err != nil {
			t.Fatalf("create session %q: %v", sid, err)
		}
	}
	store := transcript.NewStore(svc, appName)
	obs := observability.New()
	return &a2aBackend{
		store:        store,
		obs:          obs,
		tracker:      newTurnTracker(store, discardLogger(), obs, "(test)"),
		logger:       discardLogger(),
		workloadName: "triage",
	}, store
}

func TestA2ABackendGetTask(t *testing.T) {
	b, store := newA2ABackend(t, "live", "gone-terminal")
	ctx := context.Background()

	// A reserved ops-row id is never addressable as a task.
	if _, err := b.GetTask(ctx, "live:mast-ops"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("GetTask(reserved) err = %v, want ErrTaskNotFound", err)
	}
	// An unknown session maps ErrNotFound → ErrTaskNotFound.
	if _, err := b.GetTask(ctx, "nonexistent"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("GetTask(unknown) err = %v, want ErrTaskNotFound", err)
	}
	// An idle session is "working" (the log can't prove completion) and is
	// stamped with the backend's workload.
	info, err := b.GetTask(ctx, "live")
	if err != nil {
		t.Fatalf("GetTask(live): %v", err)
	}
	if info.State != a2a.TaskStateWorking || info.WorkloadName != "triage" {
		t.Fatalf("GetTask(live) = %+v, want working/triage", info)
	}
	// An aborted session projects to canceled with its reason.
	if err := store.Abort(ctx, "", "gone-terminal", "operator abort"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	info, err = b.GetTask(ctx, "gone-terminal")
	if err != nil {
		t.Fatalf("GetTask(aborted): %v", err)
	}
	if info.State != a2a.TaskStateCanceled || info.StatusMessage != "operator abort" {
		t.Fatalf("GetTask(aborted) = %+v, want canceled/operator abort", info)
	}
}

func TestA2ABackendCancelTask(t *testing.T) {
	b, _ := newA2ABackend(t, "s1")
	ctx := context.Background()

	// Reserved id and unknown id both refuse as not-found.
	if _, err := b.CancelTask(ctx, "s1:mast-ops", "x"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("CancelTask(reserved) err = %v, want ErrTaskNotFound", err)
	}
	if _, err := b.CancelTask(ctx, "ghost", "x"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("CancelTask(unknown) err = %v, want ErrTaskNotFound", err)
	}

	// First cancel lands the terminal marker and reports canceled.
	info, err := b.CancelTask(ctx, "s1", "a2a tasks/cancel")
	if err != nil {
		t.Fatalf("CancelTask(s1): %v", err)
	}
	if info.State != a2a.TaskStateCanceled {
		t.Fatalf("first cancel state = %q, want canceled", info.State)
	}

	// Second cancel is idempotent: ErrAlreadyAborted maps to success and
	// still reports canceled (no spurious error surfaces to the caller).
	info, err = b.CancelTask(ctx, "s1", "a2a tasks/cancel")
	if err != nil {
		t.Fatalf("second CancelTask(s1): %v", err)
	}
	if info.State != a2a.TaskStateCanceled {
		t.Fatalf("idempotent cancel state = %q, want canceled", info.State)
	}
}

// TestA2AOutcomeVocabularyMatchesTaskStates pins the "must not drift"
// contract between the observability outcome constants (the primed label
// set for mast_a2a_server_tasks_total) and the pkg/a2a TaskState wire
// values the server records through them.
func TestA2AOutcomeVocabularyMatchesTaskStates(t *testing.T) {
	pairs := []struct {
		outcome string
		state   a2a.TaskState
	}{
		{observability.A2ATaskSubmitted, a2a.TaskStateSubmitted},
		{observability.A2ATaskWorking, a2a.TaskStateWorking},
		{observability.A2ATaskInputRequired, a2a.TaskStateInputRequired},
		{observability.A2ATaskCompleted, a2a.TaskStateCompleted},
		{observability.A2ATaskFailed, a2a.TaskStateFailed},
		{observability.A2ATaskCanceled, a2a.TaskStateCanceled},
		{observability.A2ATaskRejected, a2a.TaskStateRejected},
	}
	for _, p := range pairs {
		if p.outcome != string(p.state) {
			t.Fatalf("outcome %q != TaskState %q (the metric vocabulary drifted from the wire states)", p.outcome, p.state)
		}
	}
}

func TestMapTranscriptState(t *testing.T) {
	cases := []struct {
		name      string
		detail    *transcript.Detail
		wantState a2a.TaskState
		wantMsg   string
	}{
		{
			name:      "aborted maps to canceled",
			detail:    &transcript.Detail{Summary: transcript.Summary{State: transcript.StateAborted, AbortReason: "operator abort"}},
			wantState: a2a.TaskStateCanceled,
			wantMsg:   "operator abort",
		},
		{
			name: "paused with pending interrupt maps to input-required",
			detail: &transcript.Detail{Summary: transcript.Summary{
				State:               transcript.StatePaused,
				PendingInterruptIDs: []string{"i1"},
				PauseMessage:        "approve?",
			}},
			wantState: a2a.TaskStateInputRequired,
			wantMsg:   "approve?",
		},
		{
			name: "gate-only pause maps to working",
			detail: &transcript.Detail{Summary: transcript.Summary{
				State:        transcript.StatePaused,
				PauseMessage: "timed hold",
			}},
			wantState: a2a.TaskStateWorking,
			wantMsg:   "timed hold",
		},
		{
			name:      "interrupted maps to working",
			detail:    &transcript.Detail{Summary: transcript.Summary{State: transcript.StateInterrupted, InterruptReason: "shutdown"}},
			wantState: a2a.TaskStateWorking,
			wantMsg:   "shutdown",
		},
		{
			name:      "idle maps to working (never completed from the log)",
			detail:    &transcript.Detail{Summary: transcript.Summary{State: transcript.StateIdle}},
			wantState: a2a.TaskStateWorking,
			wantMsg:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, msg := mapTranscriptState(tc.detail)
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q", state, tc.wantState)
			}
			if msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", msg, tc.wantMsg)
			}
		})
	}
}

func TestMapTranscriptStateNeverCompleted(t *testing.T) {
	// The transcript cannot prove a turn finished; no projection may ever
	// return the "completed" A2A state (that comes from Stage B's registry).
	for _, s := range []string{transcript.StateIdle, transcript.StatePaused, transcript.StateInterrupted, transcript.StateAborted} {
		if got, _ := mapTranscriptState(&transcript.Detail{Summary: transcript.Summary{State: s}}); got == a2a.TaskStateCompleted {
			t.Fatalf("state %q mapped to completed", s)
		}
	}
}

func TestA2AExposedSkills(t *testing.T) {
	// Not opted in → no skills.
	if got := a2aExposedSkills(&workload.Bundle{Name: "w"}); got != nil {
		t.Fatalf("expose:false: got %v, want nil", got)
	}
	if got := a2aExposedSkills(nil); got != nil {
		t.Fatalf("nil bundle: got %v, want nil", got)
	}

	// Opted in with defaults: skill name and description fall back to the
	// workload name/description.
	b := &workload.Bundle{
		Name:        "triage",
		Description: "GKE triage",
		A2A:         workload.A2A{Expose: true, Auth: workload.A2AAuth{Scopes: []string{"triage:invoke"}}},
	}
	got := a2aExposedSkills(b)
	if len(got) != 1 {
		t.Fatalf("got %d skills, want 1", len(got))
	}
	sk := got[0]
	if sk.WorkloadName != "triage" || sk.SkillName != "triage" || sk.Description != "GKE triage" {
		t.Fatalf("skill = %+v", sk)
	}
	if len(sk.Scopes) != 1 || sk.Scopes[0] != "triage:invoke" {
		t.Fatalf("scopes = %v", sk.Scopes)
	}

	// Explicit skill name/description win.
	b.A2A.SkillName = "gke-triage"
	b.A2A.SkillDescription = "triage alerts"
	got = a2aExposedSkills(b)
	if got[0].SkillName != "gke-triage" || got[0].Description != "triage alerts" {
		t.Fatalf("explicit skill = %+v", got[0])
	}
}

func TestA2AValidator(t *testing.T) {
	skills := []a2a.ExposedSkill{
		{WorkloadName: "a", SkillName: "a", Scopes: []string{"a:invoke", "shared"}},
		{WorkloadName: "b", SkillName: "b", Scopes: []string{"b:invoke", "shared"}},
	}
	logger := newLogger("error")

	// Unset token → no validator (dev-only open access).
	t.Setenv("MAST_A2A_TOKEN", "")
	v, err := a2aValidator(logger, skills)
	if err != nil {
		t.Fatalf("a2aValidator(unset): %v", err)
	}
	if v != nil {
		t.Fatal("unset token: want nil validator")
	}

	// Set token → static validator whose principal carries the union of
	// every exposed skill's scopes.
	t.Setenv("MAST_A2A_TOKEN", "secret")
	v, err = a2aValidator(logger, skills)
	if err != nil {
		t.Fatalf("a2aValidator(set): %v", err)
	}
	p, err := v.Validate(t.Context(), "secret")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, want := range []string{"a:invoke", "b:invoke", "shared"} {
		if !hasScopeTest(p.Scopes, want) {
			t.Fatalf("principal missing scope %q (have %v)", want, p.Scopes)
		}
	}
	// "shared" appears in both skills but must be de-duplicated.
	if n := countTest(p.Scopes, "shared"); n != 1 {
		t.Fatalf("scope \"shared\" appears %d times, want 1", n)
	}
}

func hasScopeTest(scopes []string, want string) bool {
	return countTest(scopes, want) > 0
}

func countTest(scopes []string, want string) int {
	n := 0
	for _, s := range scopes {
		if s == want {
			n++
		}
	}
	return n
}
