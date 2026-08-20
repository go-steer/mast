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

// Tests for who a durable HITL approval names (#194). PauseRecord.
// ConsumedBy is the audit answer to "who approved this?" — the reason a
// durable gate beats an in-memory prompt — and the daemon used to write
// the constant "operator resume --token" into it on every HTTP resume,
// naming the channel rather than the human. pkg/inject already resolves
// the caller onto the request context (pkg/inject/caller_test.go covers
// that half); these cover the half that reads it.
package main

import (
	"context"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/transcript"
)

// callerCtx is what pkg/inject's handleResume hands the daemon.
func callerCtx(identity string) context.Context {
	return auth.WithCaller(context.Background(), auth.Caller{Identity: identity})
}

// TestResumeByTokenNamesTheApprover_GatePlane: a gate-pause resume IS
// the consume, so the identity has exactly one place to land.
func TestResumeByTokenNamesTheApprover_GatePlane(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-gate-attrib")
	store := transcript.NewStore(svc, appName)

	handle, _, err := store.PauseGate(context.Background(), "", "s-gate-attrib", transcript.PauseSpec{
		Reason: transcript.ReasonMaintenanceWindow,
	})
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}

	resumeByToken := newResumeByToken(store, discardLogger(), func(context.Context, inject.ResumeRequest) error {
		t.Error("gate-plane resume ran a turn; nothing was parked")
		return nil
	})

	ctx := callerCtx("alice@example.com")
	if err := resumeByToken(ctx, inject.ResumeRequest{Token: handle.Token}); err != nil {
		t.Fatalf("resumeByToken: %v", err)
	}

	rec := findRecord(t, store, "s-gate-attrib", handle.Token)
	if rec.ConsumedBy != "alice@example.com" {
		t.Errorf("ConsumedBy = %q, want %q — the audit record names the channel, not the human", rec.ConsumedBy, "alice@example.com")
	}
	if rec.ConsumedAt.IsZero() {
		t.Error("ConsumedAt is zero: the token was not consumed at all")
	}
}

// TestResumeByTokenNamesTheApprover_InterruptPlane: the interrupt path
// has TWO places the identity has to reach — the post-append consume,
// and the default response the resumed turn (and therefore the model)
// sees. The second used to read {"resumed_by": "operator"}.
func TestResumeByTokenNamesTheApprover_InterruptPlane(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-int-attrib")
	store := transcript.NewStore(svc, appName)

	handle, err := store.PauseInterrupt(context.Background(), "", "s-int-attrib", "approve-rollback", transcript.PauseSpec{
		Reason: transcript.ReasonAmbiguity,
	})
	if err != nil {
		t.Fatalf("PauseInterrupt: %v", err)
	}

	var seen inject.ResumeRequest
	resumeByToken := newResumeByToken(store, discardLogger(), func(_ context.Context, req inject.ResumeRequest) error {
		seen = req
		return nil
	})

	ctx := callerCtx("alice@example.com")
	if err := resumeByToken(ctx, inject.ResumeRequest{Token: handle.Token}); err != nil {
		t.Fatalf("resumeByToken: %v", err)
	}

	resp, ok := seen.Response.(map[string]any)
	if !ok {
		t.Fatalf("inner response is %T, want map[string]any", seen.Response)
	}
	if resp["resumed_by"] != "alice@example.com" {
		t.Errorf("default response resumed_by = %v, want %q", resp["resumed_by"], "alice@example.com")
	}

	rec := findRecord(t, store, "s-int-attrib", handle.Token)
	if rec.ConsumedBy != "alice@example.com" {
		t.Errorf("ConsumedBy = %q, want %q", rec.ConsumedBy, "alice@example.com")
	}
}

// TestResumeByTokenKeepsAnExplicitResponse: naming the approver must
// not start overwriting an answer the operator actually supplied — the
// identity only fills the default.
func TestResumeByTokenKeepsAnExplicitResponse(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-explicit-resp")
	store := transcript.NewStore(svc, appName)

	handle, err := store.PauseInterrupt(context.Background(), "", "s-explicit-resp", "approve-rollback", transcript.PauseSpec{
		Reason: transcript.ReasonAmbiguity,
	})
	if err != nil {
		t.Fatalf("PauseInterrupt: %v", err)
	}

	var seen inject.ResumeRequest
	resumeByToken := newResumeByToken(store, discardLogger(), func(_ context.Context, req inject.ResumeRequest) error {
		seen = req
		return nil
	})

	want := map[string]any{"approved": true, "note": "rollback approved by oncall"}
	if err := resumeByToken(callerCtx("alice@example.com"), inject.ResumeRequest{
		Token:    handle.Token,
		Response: want,
	}); err != nil {
		t.Fatalf("resumeByToken: %v", err)
	}

	resp, ok := seen.Response.(map[string]any)
	if !ok {
		t.Fatalf("inner response is %T, want map[string]any", seen.Response)
	}
	if resp["approved"] != true || resp["note"] != "rollback approved by oncall" {
		t.Errorf("operator's response was not passed through: %+v", resp)
	}
	if _, injected := resp["resumed_by"]; injected {
		t.Error("resumed_by was injected into an operator-supplied response; the default is for the empty case only")
	}
	// The approver still reaches the audit field — that comes from the
	// credential, not from the body, so an explicit response cannot
	// suppress it.
	if rec := findRecord(t, store, "s-explicit-resp", handle.Token); rec.ConsumedBy != "alice@example.com" {
		t.Errorf("ConsumedBy = %q, want %q", rec.ConsumedBy, "alice@example.com")
	}
}

// TestResumeByTokenProxiedApprovalNamesBoth is the switchboard shape: a
// relay authenticates with its own credential and asserts the human who
// pressed the button. Recording either alone loses half the answer.
func TestResumeByTokenProxiedApprovalNamesBoth(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-proxy-attrib")
	store := transcript.NewStore(svc, appName)

	handle, _, err := store.PauseGate(context.Background(), "", "s-proxy-attrib", transcript.PauseSpec{
		Reason: transcript.ReasonMaintenanceWindow,
	})
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}

	resumeByToken := newResumeByToken(store, discardLogger(), nil)
	ctx := auth.WithProxyBy(callerCtx("alice@example.com"), "sa:switchboard")
	if err := resumeByToken(ctx, inject.ResumeRequest{Token: handle.Token}); err != nil {
		t.Fatalf("resumeByToken: %v", err)
	}

	const want = "alice@example.com (asserted by sa:switchboard)"
	if rec := findRecord(t, store, "s-proxy-attrib", handle.Token); rec.ConsumedBy != want {
		t.Errorf("ConsumedBy = %q, want %q", rec.ConsumedBy, want)
	}
}

// TestResumeByTokenWithoutACallerNamesTheMechanism: the daemon path
// always attributes at least the shared credential, so a caller-less
// context means an in-process caller. Naming the mechanism is the
// truthful answer; inventing a human is not.
func TestResumeByTokenWithoutACallerNamesTheMechanism(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-nocaller-attrib")
	store := transcript.NewStore(svc, appName)

	handle, _, err := store.PauseGate(context.Background(), "", "s-nocaller-attrib", transcript.PauseSpec{
		Reason: transcript.ReasonMaintenanceWindow,
	})
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}

	resumeByToken := newResumeByToken(store, discardLogger(), nil)
	if err := resumeByToken(context.Background(), inject.ResumeRequest{Token: handle.Token}); err != nil {
		t.Fatalf("resumeByToken: %v", err)
	}

	if rec := findRecord(t, store, "s-nocaller-attrib", handle.Token); rec.ConsumedBy != "mast:internal" {
		t.Errorf("ConsumedBy = %q, want %q", rec.ConsumedBy, "mast:internal")
	}
}

// TestResumeByTokenExpiredDoesNotConsume: the attribution change must
// not have moved the consume ahead of the expiry check — an expired
// token refuses and leaves the pause (and the record) intact.
//
// Unlike the five above, this one PASSES on the pre-fix code, and is
// meant to: it guards the ordering the refactor could have disturbed,
// not the identity the refactor introduced. Neutralizing the
// attribution leaves it green, which is the correct result for it.
func TestResumeByTokenExpiredDoesNotConsume(t *testing.T) {
	svc := adksession.InMemoryService()
	seedSession(t, svc, "s-expired-attrib")
	store := transcript.NewStore(svc, appName)

	handle, _, err := store.PauseGate(context.Background(), "", "s-expired-attrib", transcript.PauseSpec{
		Reason:   transcript.ReasonMaintenanceWindow,
		TokenTTL: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	resumeByToken := newResumeByToken(store, discardLogger(), nil)
	if err := resumeByToken(callerCtx("alice@example.com"), inject.ResumeRequest{Token: handle.Token}); err == nil {
		t.Fatal("expired token resumed without error")
	}
	if rec := findRecord(t, store, "s-expired-attrib", handle.Token); !rec.ConsumedAt.IsZero() {
		t.Errorf("expired token was consumed anyway (by %q)", rec.ConsumedBy)
	}
}

func findRecord(t *testing.T, store *transcript.Store, sessionID, token string) *transcript.PauseRecord {
	t.Helper()
	recs, err := store.PauseRecords(context.Background(), "", sessionID)
	if err != nil {
		t.Fatalf("PauseRecords: %v", err)
	}
	// Keyed by plane/interrupt, not by token — match on the field.
	for _, rec := range recs {
		if rec.Token == token {
			return rec
		}
	}
	t.Fatalf("no pause record for token %q (have %d)", token, len(recs))
	return nil
}
