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

package approval

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The verdict vocabulary as a wire contract (v0.5 W6.2).
//
// A verdict is produced in another repo — switchboard renders the
// Approve / Reject / Edit buttons and posts what the operator pressed to
// mast's /resume — so these constant VALUES are an interface, not an
// implementation detail. Renaming the Go identifiers is free; changing
// the strings is a break in a repo this compiler cannot see, and the
// failure mode is a verdict refused as unknown while an operator watches
// their approval do nothing.
//
// pkg/inject's wirecontract_test.go pins the envelope these ride in.
// This file pins the payload.

func TestWireContract_OutcomeVocabulary(t *testing.T) {
	for _, tc := range []struct {
		got  Outcome
		want string
	}{
		{OutcomeApprove, "approve"},
		{OutcomeReject, "reject"},
		{OutcomeEdit, "edit"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("outcome = %q, want %q — switchboard posts this literal", tc.got, tc.want)
		}
	}
}

// TestWireContract_ScopeVocabulary pins the scopes a client may ask for.
// A mutation only ever gets `once` (permissions.Gate refuses the rest by
// name), but the refusal has to recognize what it is refusing: an
// unknown scope and a too-broad scope are different answers, and a
// client that asks for `session` deserves the second one.
func TestWireContract_ScopeVocabulary(t *testing.T) {
	for _, tc := range []struct {
		got  Scope
		want string
	}{
		{ScopeOnce, "once"},
		{ScopeSession, "session"},
		{ScopeSessionTool, "session_tool"},
		{ScopeAlways, "always"},
		{ScopeChangeSet, "change_set"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("scope = %q, want %q", tc.got, tc.want)
		}
	}
}

// TestWireContract_VerdictFields pins the JSON names on the payload
// itself. `approver` is here for completeness and NOT because a client
// should send one: cmd/mast's verdictFor overwrites whatever arrives
// with the authenticated principal (#194). It stays on the struct
// because mast serializes verdicts too — into the decision export, where
// the field is the answer to who approved what.
func TestWireContract_VerdictFields(t *testing.T) {
	rt := reflect.TypeOf(Verdict{})
	var got []string
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"approver", "args", "note", "scope", "verdict"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Verdict wire fields = %v, want %v", got, want)
	}
}

// TestWireContract_ConfirmationEnvelope pins the two keys ADK's
// RequestConfirmationRequestProcessor reads before it re-dispatches a
// parked call. `confirmed` is ADK's; `payload` is where mast's verdict
// rides, because the boolean cannot express an edit. Get either name
// wrong and the resume looks accepted and re-dispatches nothing.
func TestWireContract_ConfirmationEnvelope(t *testing.T) {
	env := ConfirmationResponse(Verdict{Verdict: OutcomeEdit, Args: map[string]any{"replicas": 2}})
	confirmed, ok := env["confirmed"].(bool)
	if !ok {
		t.Fatalf("envelope[confirmed] = %T, want bool", env["confirmed"])
	}
	// An edit is confirmed=true: the operator is authorizing a call,
	// just not the one the model proposed.
	if !confirmed {
		t.Error("an edit came back confirmed=false; ADK would drop the call rather than re-dispatch it with the operator's arguments")
	}
	if _, ok := env["payload"]; !ok {
		t.Fatalf("envelope has no payload key; got %v", keysOf(env))
	}
	if len(env) != 2 {
		t.Errorf("envelope has %d keys (%v), want exactly confirmed and payload", len(env), keysOf(env))
	}
	// And the payload survives a JSON round trip as the verdict, which
	// is the form it actually reaches ADK in.
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var back struct {
		Confirmed bool    `json:"confirmed"`
		Payload   Verdict `json:"payload"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if back.Payload.Verdict != OutcomeEdit {
		t.Errorf("payload verdict = %q after a round trip, want edit", back.Payload.Verdict)
	}
	if got := back.Payload.Args["replicas"]; got == nil {
		t.Error("the edited arguments did not survive the round trip; an edit with no arguments is an approval of the call the operator declined")
	}
}

// TestWireContract_RejectIsNotConfirmed is the other half, and the one
// with teeth: a reject that serialized as confirmed=true would execute
// the mutation the operator refused.
func TestWireContract_RejectIsNotConfirmed(t *testing.T) {
	env := ConfirmationResponse(Verdict{Verdict: OutcomeReject})
	if env["confirmed"] != false {
		t.Fatalf("envelope[confirmed] = %v for a reject, want false", env["confirmed"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
