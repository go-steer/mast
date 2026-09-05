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
	"strings"
	"testing"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/approval"
)

// The read half of #296: projecting prior-state captures out of the
// event log, so `mast sessions show` can tell an operator what a change
// overwrote and what puts it back.

func captureEvent(callID string, r approval.CaptureRecord) *adksession.Event {
	raw, err := approval.EncodeCapture(r)
	if err != nil {
		panic(err)
	}
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = "sre"
	ev.Actions.StateDelta = map[string]any{approval.CaptureStateKey(callID): raw}
	return ev
}

func TestGetCaptures(t *testing.T) {
	scaled := approval.CaptureRecord{
		Tool:        "scale_deployment",
		Key:         "scale_deployment(deployment=api, replicas=10)",
		Read:        "get_deployment",
		ReadArgs:    map[string]any{"name": "api"},
		Prior:       map[string]any{"spec.replicas": float64(3)},
		PriorFields: []string{"spec.replicas"},
		Digest:      "d8df381d496ca5ee",
		Revert: &approval.ProposedChange{
			Tool:      "scale_deployment",
			Arguments: map[string]any{"deployment": "api", "replicas": float64(3)},
		},
	}
	// A capture with no revert: the prior state is known, the call that
	// restores it is not. It is a complete record of a different shape,
	// not a broken one, and the show view has to carry it.
	patched := approval.CaptureRecord{
		Tool:  "patch_k8s_resource",
		Key:   "patch_k8s_resource(kind=Deployment, name=api)",
		Read:  "get_k8s_resource",
		Prior: map[string]any{"spec": map[string]any{"paused": false}},
	}

	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			seed(t, svc, "op", "s-captured",
				textEvent("agent", "triaging"),
				captureEvent("fc-1", scaled),
				captureEvent("fc-2", patched))

			store := NewStore(svc, testApp)
			d, err := store.Get(context.Background(), "", "s-captured")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if len(d.Captures) != 2 {
				t.Fatalf("Captures = %+v, want both records", d.Captures)
			}

			first := d.Captures[0]
			if first.Key != scaled.Key {
				t.Errorf("first record = %q, want %q — records must come back in the order they were taken", first.Key, scaled.Key)
			}
			if got := first.Prior["spec.replicas"]; got != float64(3) {
				t.Errorf("prior = %v, want the replica count the change overwrote", first.Prior)
			}
			if !first.Undoable() {
				t.Fatal("the record's revert did not survive the projection, so the show view would say there is no way back")
			}
			if got := first.Revert.Arguments["replicas"]; got != float64(3) {
				t.Errorf("revert would set replicas to %v, want the prior 3", got)
			}
			if d.Captures[1].Undoable() {
				t.Errorf("second record = %+v, want no revert", d.Captures[1])
			}
		})
	}
}

// TestGetCapturesRecordsAnUnreadableRowRatherThanDroppingIt is the one
// place this projection deliberately differs from the applied-edit one
// beside it, which skips what it cannot decode.
//
// The two are losing different things. A dropped edit row costs audit
// detail about a call the transcript still shows. A dropped capture row
// costs the operator the knowledge that a record was taken at all — the
// show view would say "nothing was captured", and somebody would
// conclude the change is not reversible and stop looking. A stub says
// where to dig.
func TestGetCapturesRecordsAnUnreadableRowRatherThanDroppingIt(t *testing.T) {
	good := approval.CaptureRecord{Tool: "scale_deployment", Key: "scale_deployment(deployment=api)"}

	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			seed(t, svc, "op", "s-bad-capture",
				captureEvent("fc-1", good),
				func() *adksession.Event {
					ev := textEvent("sre", "noise")
					ev.Actions.StateDelta = map[string]any{
						approval.CaptureStateKey("fc-bad"): "{not json",
						"mast_unrelated_marker":            "ignored",
					}
					return ev
				}())

			store := NewStore(svc, testApp)
			d, err := store.Get(context.Background(), "", "s-bad-capture")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if len(d.Captures) != 2 {
				t.Fatalf("Captures = %+v, want the good record AND a stub for the unreadable one", d.Captures)
			}
			stub := d.Captures[1]
			if !strings.Contains(stub.Key, "unreadable prior-state record") {
				t.Errorf("stub = %+v, want a key saying the record could not be read", stub)
			}
			if stub.FunctionCallID != "fc-bad" {
				t.Errorf("stub function call id = %q, want fc-bad so an operator can find the event", stub.FunctionCallID)
			}
			if stub.Undoable() {
				t.Error("the stub claims an undo it does not have")
			}
			if stub.CapturedAt.IsZero() {
				t.Error("the stub carries no timestamp, so nobody can tell when the lost record was taken")
			}
		})
	}
}

func TestGetNoCaptures(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			seed(t, svc, "op", "s-uncaptured", textEvent("agent", "done"))
			store := NewStore(svc, testApp)
			d, err := store.Get(context.Background(), "", "s-uncaptured")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if d.Captures != nil {
				t.Errorf("Captures = %+v, want nil so the show view prints nothing", d.Captures)
			}
		})
	}
}
