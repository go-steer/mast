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
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/genai"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/transcript"
)

// The two pause primitives that share POST /resume answer to different
// FunctionResponse names, and sending one shape to the other kind of
// pause leaves the session parked while the operator believes they
// answered it. These tests pin which shape each pause gets, and that
// the approver on the durable record is the authenticated caller's and
// not the client's.

const (
	shapeSession = "resume-shape"
	shapeConfID  = "conf-1"
	shapeInputID = "input-1"
)

// shapeFixture creates a session and appends one pending interrupt of
// the requested kind, returning a store over it.
func shapeFixture(t *testing.T, confirmation bool) *transcript.Store {
	t.Helper()
	ctx := context.Background()
	svc := adksession.InMemoryService()
	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: appName, UserID: defaultUserID, SessionID: shapeSession,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ev := adksession.NewEvent(ctx, "inv-1")
	ev.Author = "agent"
	if confirmation {
		// The shape internal/llminternal/functions.go writes: a
		// long-running adk_request_confirmation call carrying the
		// original call and mast's payload.
		ev.LongRunningToolIDs = []string{shapeConfID}
		ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				ID:   shapeConfID,
				Name: toolconfirmation.FunctionCallName,
				Args: map[string]any{
					"originalFunctionCall": map[string]any{
						"name": "scale_deployment",
						"args": map[string]any{"deployment": "api", "replicas": float64(10)},
					},
					"toolConfirmation": map[string]any{
						"hint":    "Approve mutating call scale_deployment(deployment=api, replicas=10)?",
						"payload": map[string]any{"tool": "scale_deployment"},
					},
				},
			},
		}}}
	} else {
		ev.RequestedInput = &adksession.RequestInput{
			InterruptID: shapeInputID,
			Message:     "approve the triage result?",
		}
	}
	if err := svc.AppendEvent(ctx, created.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	return transcript.NewStore(svc, appName)
}

func TestResumeMessage_ConfirmationParkGetsTheConfirmationShape(t *testing.T) {
	store := shapeFixture(t, true)
	ctx := auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"})

	msg, err := resumeMessage(ctx, store, inject.ResumeRequest{
		SessionID:   shapeSession,
		InterruptID: shapeConfID,
		Response:    map[string]any{"verdict": "approve", "note": "confirmed with on-call"},
	})
	if err != nil {
		t.Fatalf("resumeMessage: %v", err)
	}
	fr := msg.Parts[0].FunctionResponse
	if fr.Name != toolconfirmation.FunctionCallName {
		t.Fatalf("FunctionResponse.Name = %q, want %q — ADK's confirmation processor matches on this name, and any other name forks a fresh turn instead of resuming",
			fr.Name, toolconfirmation.FunctionCallName)
	}
	if fr.ID != shapeConfID {
		t.Errorf("FunctionResponse.ID = %q, want the parked call's ID %q", fr.ID, shapeConfID)
	}
	if confirmed, _ := fr.Response["confirmed"].(bool); !confirmed {
		t.Errorf("confirmed = %v, want true — ADK consults this boolean, not mast's verdict field", fr.Response["confirmed"])
	}
	v, ok := fr.Response["payload"].(approval.Verdict)
	if !ok {
		t.Fatalf("payload = %T, want an approval.Verdict", fr.Response["payload"])
	}
	if v.Verdict != approval.OutcomeApprove || v.Note != "confirmed with on-call" {
		t.Errorf("payload = %+v, want the operator's approve verdict and note", v)
	}
	if v.Approver != "alice@example.com" {
		t.Errorf("approver = %q, want the authenticated caller", v.Approver)
	}
}

func TestResumeMessage_RejectIsNotConfirmed(t *testing.T) {
	store := shapeFixture(t, true)
	msg, err := resumeMessage(context.Background(), store, inject.ResumeRequest{
		SessionID:   shapeSession,
		InterruptID: shapeConfID,
		Response:    map[string]any{"verdict": "reject"},
	})
	if err != nil {
		t.Fatalf("resumeMessage: %v", err)
	}
	if confirmed, _ := msg.Parts[0].FunctionResponse.Response["confirmed"].(bool); confirmed {
		t.Fatalf("confirmed = true for a reject verdict — ADK would re-dispatch the call and the tool would run")
	}
}

// TestResumeMessage_RequestInputParkKeepsTheLegacyShape is the other
// half of the branch: the v0.2 wire shape must not move.
func TestResumeMessage_RequestInputParkKeepsTheLegacyShape(t *testing.T) {
	store := shapeFixture(t, false)
	msg, err := resumeMessage(context.Background(), store, inject.ResumeRequest{
		SessionID:   shapeSession,
		InterruptID: shapeInputID,
		Response:    map[string]any{"approved": true},
	})
	if err != nil {
		t.Fatalf("resumeMessage: %v", err)
	}
	fr := msg.Parts[0].FunctionResponse
	if fr.Name != "adk_request_input" {
		t.Fatalf("FunctionResponse.Name = %q, want adk_request_input — workflowagent roots filter on it before matching the ID", fr.Name)
	}
	if _, ok := fr.Response["response"]; !ok {
		t.Errorf("response payload = %+v, want the answer wrapped under \"response\"", fr.Response)
	}
}

// TestResumeMessage_UnknownSessionFallsBackToRequestInput pins the
// pre-existing behaviour for anything the store cannot classify: the
// v0.2 shape, unchanged. The authoritative refusal is the runTurnPre
// chokepoint, not this function.
func TestResumeMessage_UnknownSessionFallsBackToRequestInput(t *testing.T) {
	store := shapeFixture(t, true)
	msg, err := resumeMessage(context.Background(), store, inject.ResumeRequest{
		SessionID:   "no-such-session",
		InterruptID: "whatever",
		Response:    map[string]any{"approved": true},
	})
	if err != nil {
		t.Fatalf("resumeMessage: %v", err)
	}
	if got := msg.Parts[0].FunctionResponse.Name; got != "adk_request_input" {
		t.Errorf("FunctionResponse.Name = %q, want adk_request_input", got)
	}
}

// TestResumeMessage_ClientSuppliedApproverIsOverwritten is D1: a
// verdict's approver is the authenticated caller, never the payload's
// claim. Without this the audit record says whatever the client typed.
func TestResumeMessage_ClientSuppliedApproverIsOverwritten(t *testing.T) {
	store := shapeFixture(t, true)
	ctx := auth.WithProxyBy(
		auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"}),
		"sa:slack-bot")

	msg, err := resumeMessage(ctx, store, inject.ResumeRequest{
		SessionID:   shapeSession,
		InterruptID: shapeConfID,
		Response:    map[string]any{"verdict": "approve", "approver": "cto@example.com"},
	})
	if err != nil {
		t.Fatalf("resumeMessage: %v", err)
	}
	v := msg.Parts[0].FunctionResponse.Response["payload"].(approval.Verdict)
	if strings.Contains(v.Approver, "cto@example.com") {
		t.Fatalf("approver = %q — the client's own claim reached the audit record", v.Approver)
	}
	if v.Approver != "alice@example.com (asserted by sa:slack-bot)" {
		t.Errorf("approver = %q, want the effective caller and the proxy that asserted them", v.Approver)
	}
}

// TestResumeMessage_NoCallerNamesTheMechanism covers the in-process
// resume paths (the timed-pause scheduler, boot auto-resume): there is
// no human, and the record should say so rather than be blank.
func TestResumeMessage_NoCallerNamesTheMechanism(t *testing.T) {
	store := shapeFixture(t, true)
	msg, err := resumeMessage(context.Background(), store, inject.ResumeRequest{
		SessionID:   shapeSession,
		InterruptID: shapeConfID,
		Response:    map[string]any{"verdict": "approve"},
	})
	if err != nil {
		t.Fatalf("resumeMessage: %v", err)
	}
	v := msg.Parts[0].FunctionResponse.Response["payload"].(approval.Verdict)
	if v.Approver != "mast:internal" {
		t.Errorf("approver = %q, want mast:internal", v.Approver)
	}
}

func TestResumeMessage_UnknownVerdictIsABadPayload(t *testing.T) {
	store := shapeFixture(t, true)
	_, err := resumeMessage(context.Background(), store, inject.ResumeRequest{
		SessionID:   shapeSession,
		InterruptID: shapeConfID,
		Response:    map[string]any{"verdict": "maybe"},
	})
	if err == nil {
		t.Fatalf("resumeMessage accepted an unknown verdict")
	}
	if !strings.Contains(err.Error(), inject.ErrBadPayload.Error()) {
		t.Errorf("error = %v, want it to wrap ErrBadPayload so /resume answers 400 and the client stops retrying", err)
	}
}

// TestPendingConfirmationIsOperatorLegible pins what `mast sessions
// show` (and any /resume client) sees for a parked mutating call: the
// question, and the schema of the answer. Without the enrichment the
// pause projects as an interrupt ID against an ADK-internal tool name.
func TestPendingConfirmationIsOperatorLegible(t *testing.T) {
	store := shapeFixture(t, true)
	d, err := store.Get(context.Background(), defaultUserID, shapeSession)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(d.Pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(d.Pending))
	}
	p := d.Pending[0]
	if !strings.Contains(p.Message, "scale_deployment") {
		t.Errorf("pending message = %q, want the parked call in it", p.Message)
	}
	if p.ResponseSchema == nil {
		t.Fatalf("pending has no response schema; an operator has no way to know what answer is accepted")
	}
	raw, err := json.Marshal(p.ResponseSchema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	for _, want := range []string{"approve", "reject", "edit"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("response schema %s is missing the %q verdict — the schema is durable, and widening it after a pause is a migration", raw, want)
		}
	}
}
