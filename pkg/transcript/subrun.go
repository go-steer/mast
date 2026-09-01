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

// Durable intent records for planner dispatches — the storage half of
// the v0.6 W9.3 seam (#235). See pkg/effects/subrun.go for why a
// dispatched specialist's mutating calls have nowhere else to go, and
// internal/compose/subrunrecord.go for the adapter that joins the two.
//
// # Why the companion ops row
//
// The requirement is a record that (a) an out-of-band writer can append
// while the outer runner holds a live write lease on the session it
// describes, and (b) the workload's own boot scan and next turn can
// find. The ops row is the only place in mast that is both.
//
// The primary row fails (a) for the reason opsSuffix exists: ADK's
// database session service enforces optimistic concurrency, and an
// out-of-band append invalidates the runner's handle mid-turn — which is
// precisely the moment a dispatch writes. It also fails a requirement
// nobody stated as a constraint but the dispatch shape depends on: an
// event on the primary row is an event the planner's next model round
// reads, so recording a specialist's individual tool calls there would
// hand the planner the isolation it was dispatched to avoid.
//
// A separate durable session under a derived ID fails (b): ScanInterrupted
// lists by AppName, and nothing would look for the derived one
// (pkg/transcript/dispatchscope_test.go). The ops row is already the
// session's own overlay, already read on every Get, and already skipped
// by List, ScanInterrupted and findUserID.
//
// # Why this file names no effect class
//
// The record is deliberately dumb: a tool name, a call ID, a time. What
// counts as mutating is pkg/effects' question, and this package does not
// import it — effects is the one leaf in mast's graph with no mast
// dependencies at all, and its own tests already reach the other way.
package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// subRunKeyPrefix keys one record per dispatched mutating call on the
// session's ops row. The call ID is appended raw rather than digested:
// unlike an ack key it has one variable component behind a fixed prefix,
// so the encoding is already injective, and a readable key is worth
// having when the thing an operator is debugging is a wedged session.
const subRunKeyPrefix = "mast_subrun:"

// SubRunIntent is one mutating call a dispatched specialist made on a
// runner the outbox plugin could not see.
//
// It carries the specialist's name because the log cannot: from the
// outer session's side the whole dispatch is a single invoke_specialist
// call, so "which specialist was mid-mutation when this died" is
// answerable here or nowhere.
type SubRunIntent struct {
	Tool         string    `json:"tool"`
	CallID       string    `json:"call_id"`
	InvocationID string    `json:"invocation_id,omitempty"`
	Specialist   string    `json:"specialist,omitempty"`
	At           time.Time `json:"at"`
}

// subRunKey is the ops-row state key holding one call's record.
func subRunKey(callID string) string { return subRunKeyPrefix + callID }

// RecordSubRunIntents durably records the mutating intents one dispatch
// raised, before the calls are made.
//
// One ops-row event per observed sub-run event, not per call: a model
// round that raises three mutating calls writes them together, so the
// three are durable or none are.
func (s *Store) RecordSubRunIntents(ctx context.Context, userID, sessionID string, intents []SubRunIntent) error {
	if len(intents) == 0 {
		return nil
	}
	delta := make(map[string]any, len(intents))
	names := make([]string, 0, len(intents))
	specialist := ""
	for _, in := range intents {
		if in.CallID == "" {
			// Unkeyable, therefore unpairable: recording it would create a
			// dangling record nothing can ever complete. The caller filters
			// these out already; this is the belt.
			continue
		}
		if in.At.IsZero() {
			in.At = time.Now()
		}
		in.At = in.At.UTC()
		blob, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal sub-run intent for call %q: %w", in.CallID, err)
		}
		delta[subRunKey(in.CallID)] = string(blob)
		names = append(names, in.Tool)
		specialist = in.Specialist
	}
	if len(delta) == 0 {
		return nil
	}
	text := fmt.Sprintf("specialist %q is about to call %s inside a planner dispatch",
		specialist, strings.Join(names, ", "))
	return s.appendOpsDelta(ctx, userID, sessionID, "subrun-intent", "daemon", text, delta)
}

// CompleteSubRunIntents pairs previously recorded intents.
//
// Blanked rather than deleted, the same as every other cleared marker
// here: last-write-wins is the only state semantic ADK guarantees across
// backends. Call IDs with no record are written anyway — a blank value
// reads as "not dangling" either way, and checking first would cost a
// read on the hot path to save a byte on a cold one.
func (s *Store) CompleteSubRunIntents(ctx context.Context, userID, sessionID string, callIDs []string) error {
	if len(callIDs) == 0 {
		return nil
	}
	delta := make(map[string]any, len(callIDs))
	for _, id := range callIDs {
		if id == "" {
			continue
		}
		delta[subRunKey(id)] = ""
	}
	if len(delta) == 0 {
		return nil
	}
	text := fmt.Sprintf("dispatched effect(s) completed: %s", strings.Join(callIDs, ", "))
	return s.appendOpsDelta(ctx, userID, sessionID, "subrun-complete", "daemon", text, delta)
}

// DanglingSubRunIntents returns the session's recorded-but-unpaired
// dispatched mutations, oldest first — the intents the effect outbox and
// the boot-time auto-resume pass must treat exactly as they treat a
// dangling call in the log.
//
// Overlay semantics, like every other marker read here: a missing ops
// row, an unreadable one and an undecodable record all read as "none".
// The failure that matters is the opposite one and it cannot happen this
// way round — a record that exists and decodes is always reported.
func (s *Store) DanglingSubRunIntents(ctx context.Context, userID, sessionID string) []SubRunIntent {
	var out []SubRunIntent
	for k, raw := range s.opsState(ctx, userID, sessionID) {
		if !strings.HasPrefix(k, subRunKeyPrefix) || strings.TrimSpace(raw) == "" {
			continue
		}
		var rec SubRunIntent
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	// Map iteration gave them to us in whatever order the backend's state
	// encoding happened to yield, and every consumer — the ack watermark,
	// the refusal payload, the operator log line — reads them as a
	// sequence.
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
