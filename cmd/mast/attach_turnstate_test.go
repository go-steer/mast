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
	"time"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/transcript"
)

// seedParked creates a session and appends events to it, returning a
// wiring whose store reads them back. This is the real projection —
// transcript.Store.Get over an ADK session service — not a stand-in,
// because what #313 has to get right is the mapping between two
// vocabularies and a stand-in would only assert the mapping against
// itself.
func seedParked(t *testing.T, sid string, events ...*adksession.Event) attachWiring {
	t.Helper()
	svc := adksession.InMemoryService()
	ctx := context.Background()
	resp, err := svc.Create(ctx, &adksession.CreateRequest{AppName: appName, UserID: "operator", SessionID: sid})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	base := time.Now()
	for i, ev := range events {
		ev.Timestamp = base.Add(time.Duration(i+1) * time.Second)
		if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}
	return attachWiring{store: transcript.NewStore(svc, appName)}
}

// park builds the wire shape ADK produces for a long-running tool that
// has not been answered: a FunctionCall whose ID is in
// LongRunningToolIDs.
func park(tool, callID string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = "planner"
	ev.LongRunningToolIDs = []string{callID}
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		{FunctionCall: &genai.FunctionCall{ID: callID, Name: tool, Args: map[string]any{"message": "?"}}},
	}}
	return ev
}

// The end-to-end claim #313 makes, at the only seam where both
// vocabularies are in scope. TestAttachWiringLeavesNoCapabilityUnwired
// proves TurnStateFn is non-nil; it cannot tell a correct projection
// from one that returns the wrong constant for every input.
func TestAttachWiringProjectsADurablePark(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		events []*adksession.Event
		want   string
	}{
		"write-gate approval park": {
			events: []*adksession.Event{park(toolconfirmation.FunctionCallName, "call-1")},
			want:   attach.TurnStateAwaitingPermission,
		},
		"operator-input park": {
			events: []*adksession.Event{park("request_operator_input", "call-2")},
			want:   attach.TurnStateAwaitingElicit,
		},
		"an approval outranks a question": {
			events: []*adksession.Event{
				park("request_operator_input", "call-3"),
				park(toolconfirmation.FunctionCallName, "call-4"),
			},
			want: attach.TurnStateAwaitingPermission,
		},
		"a session that is not parked": {
			events: nil,
			want:   "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := seedParked(t, "s-"+name, tc.events...)
			if got := w.turnState("s-" + name); got != tc.want {
				t.Errorf("turnState = %q, want %q", got, tc.want)
			}
		})
	}
}

// Every failure path reports "" — the pre-#313 answer — rather than
// failing the status read. An operator reaches for /status precisely
// when things are going badly; a wrong-but-coarse turn_state beats a
// 500 on the surface that would tell them so.
func TestAttachWiringTurnStateDegradesToEmpty(t *testing.T) {
	t.Parallel()

	if got := (attachWiring{}).turnState("s-nostore"); got != "" {
		t.Errorf("no store: turnState = %q, want %q", got, "")
	}

	w := seedParked(t, "s-present", park("request_operator_input", "call-1"))
	if got := w.turnState("s-does-not-exist"); got != "" {
		t.Errorf("unknown session: turnState = %q, want %q", got, "")
	}
}
