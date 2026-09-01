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
	"time"

	adksession "google.golang.org/adk/v2/session"
)

func TestSubRunIntentRoundTrip(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)
			seed(t, svc, "u1", "s1", textEvent("user", "scale it"))

			base := time.Now().Truncate(time.Millisecond)
			intents := []SubRunIntent{
				{Tool: "scale_deployment", CallID: "c1", InvocationID: "inv-9", Specialist: "remediator", At: base},
				{Tool: "restart_pod", CallID: "c2", InvocationID: "inv-9", Specialist: "remediator", At: base.Add(time.Second)},
			}
			if err := store.RecordSubRunIntents(ctx, "u1", "s1", intents); err != nil {
				t.Fatalf("RecordSubRunIntents: %v", err)
			}

			got := store.DanglingSubRunIntents(ctx, "u1", "s1")
			if len(got) != 2 {
				t.Fatalf("DanglingSubRunIntents = %d records, want 2: %+v", len(got), got)
			}
			// Oldest first, and the specialist survives the round trip —
			// from the outer session's side the whole dispatch is one
			// invoke_specialist call, so this field is the only place that
			// answers "who was mid-mutation".
			if got[0].CallID != "c1" || got[1].CallID != "c2" {
				t.Errorf("order = %q, %q; want c1, c2 (oldest first)", got[0].CallID, got[1].CallID)
			}
			if got[0].Tool != "scale_deployment" || got[0].Specialist != "remediator" || got[0].InvocationID != "inv-9" {
				t.Errorf("record 0 = %+v, want the scale_deployment intent intact", got[0])
			}
			if !got[0].At.Equal(base.UTC()) {
				t.Errorf("record 0 At = %v, want %v", got[0].At, base.UTC())
			}

			// One completion pairs one intent; the other stays dangling.
			if err := store.CompleteSubRunIntents(ctx, "u1", "s1", []string{"c1"}); err != nil {
				t.Fatalf("CompleteSubRunIntents: %v", err)
			}
			got = store.DanglingSubRunIntents(ctx, "u1", "s1")
			if len(got) != 1 || got[0].CallID != "c2" {
				t.Fatalf("after completing c1, dangling = %+v; want only c2", got)
			}

			if err := store.CompleteSubRunIntents(ctx, "u1", "s1", []string{"c2"}); err != nil {
				t.Fatalf("CompleteSubRunIntents c2: %v", err)
			}
			if got := store.DanglingSubRunIntents(ctx, "u1", "s1"); len(got) != 0 {
				t.Fatalf("after completing both, dangling = %+v; want none", got)
			}
		})
	}
}

// TestSubRunIntentSurvivesInterleavedPrimaryWrites is the reason the
// records live on the companion ops row rather than in session state:
// on the database service a session handle is a write lease, so a
// record written while a turn is in flight must not be the thing that
// kills the turn. A dispatch writes at precisely that moment.
func TestSubRunIntentSurvivesInterleavedPrimaryWrites(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)

			resp, err := svc.Create(ctx, &adksession.CreateRequest{AppName: testApp, UserID: "u1", SessionID: "s1"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			live := resp.Session // the "runner's" handle, held across the record

			first := textEvent("user", "go")
			first.Timestamp = time.Now()
			if err := svc.AppendEvent(ctx, live, first); err != nil {
				t.Fatalf("append before record: %v", err)
			}

			if err := store.RecordSubRunIntents(ctx, "u1", "s1", []SubRunIntent{
				{Tool: "scale_deployment", CallID: "c1", Specialist: "remediator", At: time.Now()},
			}); err != nil {
				t.Fatalf("RecordSubRunIntents mid-turn: %v", err)
			}

			// The held handle must still be usable — this is the assertion
			// the primary-row design would fail with a stale-session error.
			after := textEvent("model", "scaled")
			after.Timestamp = time.Now().Add(time.Second)
			if err := svc.AppendEvent(ctx, live, after); err != nil {
				t.Fatalf("append after out-of-band record: %v — the record invalidated the runner's write lease", err)
			}

			if got := store.DanglingSubRunIntents(ctx, "u1", "s1"); len(got) != 1 {
				t.Fatalf("dangling = %+v, want the one recorded intent", got)
			}
		})
	}
}

// TestSubRunIntentsAreInvisibleToTheProjection pins the isolation the
// dispatch boundary exists for: the records are operator state, not
// session content, so nothing about the projection an operator (or the
// planner's next model round) reads changes because they are there.
func TestSubRunIntentsAreInvisibleToTheProjection(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(svc, testApp)
			seed(t, svc, "u1", "s1", textEvent("user", "scale it"), textEvent("model", "done"))

			before, err := store.Get(ctx, "u1", "s1")
			if err != nil {
				t.Fatalf("Get before: %v", err)
			}
			if err := store.RecordSubRunIntents(ctx, "u1", "s1", []SubRunIntent{
				{Tool: "scale_deployment", CallID: "c1", Specialist: "remediator", At: time.Now()},
			}); err != nil {
				t.Fatalf("RecordSubRunIntents: %v", err)
			}
			after, err := store.Get(ctx, "u1", "s1")
			if err != nil {
				t.Fatalf("Get after: %v", err)
			}

			if after.State != before.State {
				t.Errorf("State %v -> %v; a dispatched-intent record must not move the session state", before.State, after.State)
			}
			if after.EventCount != before.EventCount {
				t.Errorf("EventCount %d -> %d; the record must not land in the log the planner reads", before.EventCount, after.EventCount)
			}
			if len(after.AppliedEdits) != 0 || after.LastEventTime != before.LastEventTime {
				t.Errorf("projection moved: %+v -> %+v; a dispatched-intent record is operator state, not session content", before, after)
			}

			// And it must not show up as a session of its own.
			list, err := store.List(ctx, "")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("List = %d sessions, want 1 (the ops row must stay hidden): %+v", len(list), list)
			}
		})
	}
}
