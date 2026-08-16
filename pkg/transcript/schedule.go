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

// Scheduled-trigger state — the durable half of the v0.4 W4.1 cadence
// (docs/v0.4-plan.md). One record per workload, holding the anchor its
// fires are counted from and the last tick that fired.
//
// The anchor is the whole point. A scheduled workload's next fire is
// anchor + k×interval, so a daemon that came back from a restart with
// no anchor would re-phase the schedule to whenever the process
// happened to start — "every hour" quietly becoming "an hour after
// each deploy". Persisting the anchor is what makes the cadence a
// property of the workload rather than of the process.
package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// scheduleKeyPrefix keys one schedule record per workload on the
// scheduler's ops row. Keyed by workload rather than stored as a single
// record because two daemons over two workloads may share a session DB,
// and a shared key would have each one silently re-anchoring the
// other's cadence on every boot.
const scheduleKeyPrefix = "mast_schedule:"

// scheduleRowID is the pseudo-session whose companion ops row holds the
// schedule records. It has no primary row and never will: a cadence
// belongs to the workload, not to any one of the sessions it spawns,
// and parking it on a real session would tie the schedule to that
// session's lifetime.
//
// An ops row without a primary is a supported shape — see
// MarkInterrupted, which documents the same thing for a marker that
// arrives before its session — and costs nothing operationally: List,
// ScanInterrupted, and findUserID all skip rows ending in opsSuffix, so
// the record is invisible to `mast sessions list` and cannot be
// resumed, aborted, or paused. The userID must be passed explicitly for
// exactly that reason: there is no primary row to infer it from.
const scheduleRowID = "mast-scheduler"

// ScheduleRecord is one workload's persisted cadence state.
type ScheduleRecord struct {
	// Workload names the bundle this cadence belongs to.
	Workload string `json:"workload"`

	// Interval is the declared cadence string as the bundle spelled it.
	// Diagnostic only — the daemon reads the cadence from the bundle,
	// not from here, so that editing the bundle changes the schedule.
	// Recorded because an anchor without the interval it was taken for
	// is unreadable in a support conversation.
	Interval string `json:"interval,omitempty"`

	// Anchor is the instant the cadence is counted from: the moment
	// mast first saw this workload's schedule. Every fire lands on
	// anchor + k×interval.
	Anchor time.Time `json:"anchor"`

	// LastTick is the most recent lattice point accounted for — fired,
	// or coalesced away as a missed tick. The scheduler counts skipped
	// ticks from here, so a restart reports what it skipped instead of
	// silently resuming.
	LastTick time.Time `json:"last_tick,omitempty"`

	// LastFire is when the last fire actually started (tick + jitter),
	// and Fires counts the fires this schedule has driven since the
	// anchor was set. Both are for the operator reading the record, not
	// for the arithmetic.
	LastFire time.Time `json:"last_fire,omitempty"`
	Fires    int       `json:"fires,omitempty"`
}

// Schedule reads a workload's persisted cadence state, or nil when
// there is none.
//
// A missing row, an unreadable one, and a corrupt record all read as
// "no record", the same overlay semantics every other marker read here
// uses. The consequence is worth stating plainly: the caller re-anchors
// on now, so a read blip re-phases the schedule. That is the lesser of
// the two failures — the alternative is a daemon that refuses to
// schedule anything because one read went wrong, which turns a blip
// into a trigger that never fires again.
func (s *Store) Schedule(ctx context.Context, userID, workload string) *ScheduleRecord {
	raw := s.opsState(ctx, userID, scheduleRowID)[scheduleKeyPrefix+workload]
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var rec ScheduleRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil
	}
	if rec.Anchor.IsZero() {
		// An anchor-less record cannot phase anything; treat it as
		// absent rather than as a cadence counted from the zero time,
		// which would make every tick since year 1 a missed one.
		return nil
	}
	return &rec
}

// SaveSchedule writes the workload's cadence state to the scheduler's
// ops row, replacing whatever was there.
//
// Last-write-wins is correct here and needs no read-modify-write:
// mast's scheduled trigger is single-instance (like the timed-pause
// scheduler, and for the same reason — see cmd/mast/pausesched.go),
// so the only writer of a given workload's record is the one goroutine
// that owns its cadence.
func (s *Store) SaveSchedule(ctx context.Context, userID string, rec ScheduleRecord) error {
	if userID == "" {
		return fmt.Errorf("save schedule for workload %q: userID is required (the scheduler's ops row has no primary session to infer it from)", rec.Workload)
	}
	if rec.Workload == "" {
		return fmt.Errorf("save schedule: workload name is required (it is the record's key)")
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal schedule for workload %q: %w", rec.Workload, err)
	}
	return s.appendOpsDelta(ctx, userID, scheduleRowID, "schedule", "daemon",
		fmt.Sprintf("scheduled trigger for %q anchored at %s", rec.Workload, rec.Anchor.UTC().Format(time.RFC3339Nano)),
		map[string]any{scheduleKeyPrefix + rec.Workload: string(blob)})
}
