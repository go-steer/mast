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
)

// TestScheduleRoundTrip: the anchor a restarted daemon reads back is
// the anchor the first one wrote — over both services, because the one
// that matters in production is the durable one.
func TestScheduleRoundTrip(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			ctx := context.Background()
			anchor := time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)

			if got := store.Schedule(ctx, "u1", "gke-triage"); got != nil {
				t.Fatalf("Schedule on a fresh store = %+v, want nil", got)
			}
			rec := ScheduleRecord{
				Workload: "gke-triage",
				Interval: "1h0m0s",
				Anchor:   anchor,
				LastTick: anchor.Add(2 * time.Hour),
				LastFire: anchor.Add(2*time.Hour + 12*time.Second),
				Fires:    2,
			}
			if err := store.SaveSchedule(ctx, "u1", rec); err != nil {
				t.Fatalf("SaveSchedule: %v", err)
			}
			got := store.Schedule(ctx, "u1", "gke-triage")
			if got == nil {
				t.Fatal("Schedule returned nil for a record just written")
			}
			if !got.Anchor.Equal(anchor) || !got.LastTick.Equal(rec.LastTick) || got.Fires != 2 {
				t.Errorf("Schedule = %+v, want %+v", got, rec)
			}

			// Last write wins, in place: the cadence has one owner.
			rec.LastTick = anchor.Add(3 * time.Hour)
			rec.Fires = 3
			if err := store.SaveSchedule(ctx, "u1", rec); err != nil {
				t.Fatalf("SaveSchedule (update): %v", err)
			}
			if got := store.Schedule(ctx, "u1", "gke-triage"); got.Fires != 3 || !got.LastTick.Equal(rec.LastTick) {
				t.Errorf("after update, Schedule = %+v, want the second write", got)
			}

			// One record per workload: two daemons over one store do not
			// re-anchor each other.
			other := ScheduleRecord{Workload: "cost-sweep", Anchor: anchor.Add(time.Minute)}
			if err := store.SaveSchedule(ctx, "u1", other); err != nil {
				t.Fatalf("SaveSchedule (second workload): %v", err)
			}
			if got := store.Schedule(ctx, "u1", "gke-triage"); got == nil || !got.Anchor.Equal(anchor) {
				t.Errorf("the first workload's anchor changed: %+v", got)
			}
			if got := store.Schedule(ctx, "u1", "cost-sweep"); got == nil || !got.Anchor.Equal(anchor.Add(time.Minute)) {
				t.Errorf("the second workload's anchor = %+v, want its own", got)
			}
		})
	}
}

// TestScheduleIsNotASession: the schedule lives on an ops row with no
// primary, so an operator listing sessions never sees the scheduler's
// bookkeeping among their workload's runs.
func TestScheduleIsNotASession(t *testing.T) {
	store := NewStore(services(t)["inmemory"], testApp)
	ctx := context.Background()

	if err := store.SaveSchedule(ctx, "u1", ScheduleRecord{
		Workload: "gke-triage",
		Anchor:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	list, err := store.List(ctx, "u1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("sessions list = %+v, want the schedule's ops row to be invisible", list)
	}
	if !IsReservedSessionID(opsSessionID(scheduleRowID)) {
		t.Error("the schedule's row is not in the reserved namespace; a real session could collide with it")
	}
}

// TestScheduleUnreadableRecordsReadAsAbsent: a corrupt or anchor-less
// record re-anchors rather than wedging the trigger. Re-phasing a
// cadence is recoverable; a trigger that refuses to schedule because
// one read went wrong is a workload that silently stops running.
func TestScheduleUnreadableRecordsReadAsAbsent(t *testing.T) {
	store := NewStore(services(t)["inmemory"], testApp)
	ctx := context.Background()

	for _, tc := range []struct{ name, blob string }{
		{"not JSON", "{{"},
		{"no anchor", `{"workload":"gke-triage"}`},
		{"blanked", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.appendOpsDelta(ctx, "u1", scheduleRowID, "schedule", "daemon", "corrupt",
				map[string]any{scheduleKeyPrefix + "gke-triage": tc.blob}); err != nil {
				t.Fatalf("appendOpsDelta: %v", err)
			}
			if got := store.Schedule(ctx, "u1", "gke-triage"); got != nil {
				t.Errorf("Schedule = %+v, want nil for %s", got, tc.name)
			}
		})
	}
}

// TestSaveScheduleNeedsItsOwnUserID: there is no primary row to infer
// one from, so a caller that forgets is told, not silently written
// under a guess.
func TestSaveScheduleNeedsItsOwnUserID(t *testing.T) {
	store := NewStore(services(t)["inmemory"], testApp)
	if err := store.SaveSchedule(context.Background(), "", ScheduleRecord{
		Workload: "gke-triage", Anchor: time.Now().UTC(),
	}); err == nil {
		t.Error("SaveSchedule accepted an empty userID")
	}
	if err := store.SaveSchedule(context.Background(), "u1", ScheduleRecord{
		Anchor: time.Now().UTC(),
	}); err == nil {
		t.Error("SaveSchedule accepted a record with no workload name — the record's own key")
	}
}
