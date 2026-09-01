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

package compose

import (
	"context"

	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/transcript"
)

// SubRunIntentStore joins the two halves of the v0.6 W9.3 seam (#235):
// pkg/effects decides what counts as a mutating call and when to record
// it, pkg/transcript owns the ops row it lands on, and neither imports
// the other — effects is the one package in mast's graph with no mast
// dependencies, and keeping it that way is what lets a slim embed pull
// the outbox in without the operator surface.
//
// It also binds the user ID. The store can resolve one by scanning the
// app's session list, but that scan would sit in front of every
// dispatched mutating call; a host that already knows its user ID should
// say so.
type SubRunIntentStore struct {
	Store  *transcript.Store
	UserID string
}

// RecordSubRunIntents implements effects.IntentStore.
func (s SubRunIntentStore) RecordSubRunIntents(ctx context.Context, sessionID, specialist string, intents []effects.DanglingIntent) error {
	recs := make([]transcript.SubRunIntent, 0, len(intents))
	for _, in := range intents {
		recs = append(recs, transcript.SubRunIntent{
			Tool:         in.ToolName,
			CallID:       in.CallID,
			InvocationID: in.InvocationID,
			Specialist:   specialist,
			At:           in.Timestamp,
		})
	}
	return s.Store.RecordSubRunIntents(ctx, s.UserID, sessionID, recs)
}

// CompleteSubRunIntents implements effects.IntentStore.
func (s SubRunIntentStore) CompleteSubRunIntents(ctx context.Context, sessionID string, callIDs []string) error {
	return s.Store.CompleteSubRunIntents(ctx, s.UserID, sessionID, callIDs)
}

// Dangling reads the session's recorded-but-unpaired dispatched
// mutations back in the outbox's own vocabulary. It is the function to
// hand to effects.Config.ExternalDangling and to fold into a
// DanglingScan with WithExternal.
//
// EventIndex is -1 rather than 0: these calls are not in the session's
// event log at all, and the auto-resume repair path groups by that index
// (see effects.MutatingCalls).
func (s SubRunIntentStore) Dangling(ctx context.Context, sessionID string) []effects.DanglingIntent {
	recs := s.Store.DanglingSubRunIntents(ctx, s.UserID, sessionID)
	if len(recs) == 0 {
		return nil
	}
	out := make([]effects.DanglingIntent, 0, len(recs))
	for _, r := range recs {
		out = append(out, effects.DanglingIntent{
			ToolName:     r.Tool,
			CallID:       r.CallID,
			InvocationID: r.InvocationID,
			Timestamp:    r.At,
			EventIndex:   -1,
		})
	}
	return out
}
