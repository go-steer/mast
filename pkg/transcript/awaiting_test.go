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

package transcript

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/tool/toolconfirmation"
)

// Detail.Awaiting is what lets the attach surface tell an operator
// waiting on a permission from an operator waiting on an answer. Both
// arrive as StatePaused with a long-running park, and until #313 the
// only thing downstream of them was the string "idle".
func TestAwaitingSeparatesAWriteGateParkFromAQuestion(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			ctx := context.Background()

			seed(t, svc, "u1", "s-gatepark",
				textEvent("user", "scale the deployment"),
				parkEvent("planner", toolconfirmation.FunctionCallName, "call-conf-1", "scale nginx to 0"),
			)
			seed(t, svc, "u1", "s-question",
				textEvent("user", "which cluster?"),
				parkEvent("planner", "request_operator_input", "call-ask-1", "staging or prod?"),
			)

			gate, err := store.Get(ctx, "", "s-gatepark")
			if err != nil {
				t.Fatalf("Get s-gatepark: %v", err)
			}
			if got := gate.Awaiting(); got != AwaitingApproval {
				t.Errorf("write-gate park: Awaiting() = %q, want %q (pending = %+v)", got, AwaitingApproval, gate.Pending)
			}

			ask, err := store.Get(ctx, "", "s-question")
			if err != nil {
				t.Fatalf("Get s-question: %v", err)
			}
			if got := ask.Awaiting(); got != AwaitingInput {
				t.Errorf("operator-input park: Awaiting() = %q, want %q (pending = %+v)", got, AwaitingInput, ask.Pending)
			}

			// Resolving the park ends the wait. A state nothing leaves
			// is as useless to a client as one nothing enters.
			appendTo(t, svc, "u1", "s-gatepark", resolutionEvent("call-conf-1"))
			gate, err = store.Get(ctx, "", "s-gatepark")
			if err != nil {
				t.Fatalf("Get after resolve: %v", err)
			}
			if got := gate.Awaiting(); got != "" {
				t.Errorf("after the verdict landed: Awaiting() = %q, want %q", got, "")
			}
		})
	}
}

// An abort resolves the park by force rather than by an answer, and
// #313's "done when" names that case explicitly: the session must
// leave the awaiting state either way.
func TestAwaitingEndsWhenAParkIsAborted(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			ctx := context.Background()
			seed(t, svc, "u1", "s-abortpark",
				parkEvent("planner", toolconfirmation.FunctionCallName, "call-conf-2", "delete the namespace"),
			)
			if got := mustGet(t, store, "s-abortpark").Awaiting(); got != AwaitingApproval {
				t.Fatalf("premise: Awaiting() = %q, want %q before the abort", got, AwaitingApproval)
			}
			if err := store.Abort(ctx, "u1", "s-abortpark", "operator gave up"); err != nil {
				t.Fatalf("Abort: %v", err)
			}
			if got := mustGet(t, store, "s-abortpark").Awaiting(); got != "" {
				t.Errorf("after abort: Awaiting() = %q, want %q", got, "")
			}
		})
	}
}

// A gate pause is StatePaused with nothing pending. It is a hold an
// operator placed, not a question the agent asked — there is no answer
// to give it, so it reports no wait. Getting this wrong would put a
// prompt in front of an operator with nothing to type into it.
func TestAwaitingIsEmptyForAGateOnlyPause(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			ctx := context.Background()
			seed(t, svc, "u1", "s-hold", textEvent("user", "hi"))
			if _, _, err := store.PauseGate(ctx, "", "s-hold", PauseSpec{Reason: ReasonOperator, Message: "hold for review"}); err != nil {
				t.Fatalf("PauseGate: %v", err)
			}
			d := mustGet(t, store, "s-hold")
			if d.State != StatePaused {
				t.Fatalf("premise: state = %q, want %q", d.State, StatePaused)
			}
			if got := d.Awaiting(); got != "" {
				t.Errorf("gate-only pause: Awaiting() = %q, want %q", got, "")
			}
		})
	}
}

// A write-gate park outranks a plain question when both are open: it
// is the one that authorizes a mutating call, and a client that can
// render one prompt should render that one.
func TestAwaitingRanksApprovalAboveAQuestion(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			// Question first, so a naive "first pending wins" ordering
			// would pick the wrong one.
			seed(t, svc, "u1", "s-both",
				parkEvent("planner", "request_operator_input", "call-ask-2", "which cluster?"),
				parkEvent("planner", toolconfirmation.FunctionCallName, "call-conf-3", "scale nginx to 0"),
			)
			d := mustGet(t, store, "s-both")
			if len(d.Pending) != 2 {
				t.Fatalf("premise: pending = %+v, want both parks open", d.Pending)
			}
			if got := d.Awaiting(); got != AwaitingApproval {
				t.Errorf("both open: Awaiting() = %q, want %q", got, AwaitingApproval)
			}
		})
	}
}

// An idle session is not waiting on anybody.
func TestAwaitingIsEmptyForAnUnpausedSession(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			seed(t, svc, "u1", "s-idle", textEvent("user", "hi"), textEvent("agent", "hello"))
			if got := mustGet(t, store, "s-idle").Awaiting(); got != "" {
				t.Errorf("idle session: Awaiting() = %q, want %q", got, "")
			}
		})
	}
}

// The StatePaused guard, measured on its own. Worth stating what this
// does and does not prove: no projection Store.Get can produce reaches
// it, because every path that sets a non-paused state also empties
// Pending (Abort nils it; a pending interrupt forces StatePaused), so
// removing the guard leaves every other test in this file green — it
// was checked. Detail is an exported struct with an exported method,
// though, and State is the authority over Pending for a caller that
// holds one. This test is the only thing that says so.
func TestAwaitingDefersToStateOverPending(t *testing.T) {
	d := Detail{
		Summary: Summary{State: StateAborted, AbortReason: "operator gave up"},
		Pending: []PendingInput{{InterruptID: "call-1", ToolName: toolconfirmation.FunctionCallName}},
	}
	if got := d.Awaiting(); got != "" {
		t.Errorf("aborted session carrying a stale park: Awaiting() = %q, want %q — an aborted session is not resumable, so prompting for a verdict on it asks for something nobody can act on", got, "")
	}
}

func mustGet(t *testing.T, store *Store, sid string) *Detail {
	t.Helper()
	d, err := store.Get(context.Background(), "", sid)
	if err != nil {
		t.Fatalf("Get %q: %v", sid, err)
	}
	return d
}
