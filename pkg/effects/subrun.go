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

package effects

// Out-of-band recording for a planner dispatch (#235, v0.6 W9.3).
//
// # What this closes, and what it does not
//
// invoke_specialist runs its specialist on a runner it constructs in the
// tool body, and runner plugins are runner-scoped, so neither the write
// gate nor the outbox plugin reaches a mutating call made inside a
// dispatch. Two halves of that hole have different answers.
//
// The GATE half is permanent and is refused at composition instead
// (internal/compose.CheckPlannerWriteSurface): an approval is a round
// trip, the answer comes back by matching a session event log, and a
// dispatch's sub-session is an in-memory throwaway nobody re-enters.
//
// The RECORDING half is what this file closes. Recording is
// one-directional, so the resume obstacle does not apply, and the sink
// planner already opens per dispatch (planner.SubRunObserver, #226) sees
// a mutating FunctionCall BEFORE the tool body runs — measured in
// pkg/planner/outboxseam_test.go, asserted rather than logged because
// ADK owns that ordering.
//
// # Why the record does not go in the session log
//
// The outbox's usual store is the session event log itself ("this
// package adds no storage"), and that is unavailable here twice over.
//
// Writing the sub-run's calls into the OUTER session's log would put a
// specialist's individual tool calls in front of the planner's model,
// which is the isolation the dispatch shape exists to provide. And an
// out-of-band append to a live session row is exactly what ADK's
// database session service refuses: a session handle is a write lease,
// and the outer runner is holding one for the turn this dispatch is part
// of (see transcript.opsSuffix — core-agent hit the identical failure
// and fixed it the same way).
//
// Writing into the sub-session's own log buys nothing either: it is
// in-memory and dies with the tool call, and even a durable one under a
// derived ID would be invisible to the workload's boot scan, which lists
// by AppName and the sub-runner's is "planner_dispatch"
// (pkg/transcript/dispatchscope_test.go).
//
// So the record goes to an IntentStore — in mast's own hosts, the
// session's companion ops row, the same place every other out-of-band
// marker lives — and reaches the two consumers that act on dangling
// intents through Config.ExternalDangling and DanglingScan.WithExternal.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

// subRunRecordTimeout bounds one store round trip. Deliberately short:
// the intent write sits in front of a mutating call, so every
// millisecond of it is latency a specialist pays.
const subRunRecordTimeout = 5 * time.Second

// IntentStore persists a sub-run's mutating intents somewhere the
// workload's own session can be scanned for them later.
//
// Both methods take the OUTER session's ID. The sub-run's own session is
// a throwaway nobody can name afterwards, so filing anything under it
// would be filing it where no operator and no boot pass will look.
type IntentStore interface {
	// RecordSubRunIntents durably records intents raised by one
	// specialist inside sessionID. It must return an error if the
	// records are not durable, because the caller treats that as a
	// reason not to let the call proceed.
	RecordSubRunIntents(ctx context.Context, sessionID, specialist string, intents []DanglingIntent) error

	// CompleteSubRunIntents marks previously recorded intents paired.
	// Unknown call IDs are not an error: a completion whose intent was
	// never recorded is a call this recorder did not classify mutating.
	CompleteSubRunIntents(ctx context.Context, sessionID string, callIDs []string) error
}

// SubRunRecorderConfig configures a recorder.
type SubRunRecorderConfig struct {
	// Store is where records land; required.
	Store IntentStore

	// SessionID is the OUTER session the dispatch belongs to; required.
	// A recorder built with an empty one refuses every event rather than
	// filing spend and effects under a session named "".
	SessionID string

	// Specialist is the roster name being dispatched, for attribution.
	Specialist string

	// Predicate classifies tools; required.
	Predicate Predicate

	// SubAgentNames excludes task delegations named after a composed
	// agent, exactly as the log scan does. Optional.
	SubAgentNames map[string]bool

	// Logger records completion-write failures, which are swallowed.
	// Optional.
	Logger *slog.Logger
}

// NewSubRunRecorder builds the recording half of the dispatch seam. The
// result satisfies planner.SubRunSink structurally; this package does
// not import pkg/planner, which keeps the outbox's dependency graph the
// leaf it has always been.
func NewSubRunRecorder(cfg SubRunRecorderConfig) (*SubRunRecorder, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("effects: SubRunRecorderConfig.Store is required")
	}
	if cfg.Predicate == nil {
		return nil, fmt.Errorf("effects: SubRunRecorderConfig.Predicate is required")
	}
	if cfg.SessionID == "" {
		return nil, fmt.Errorf("effects: SubRunRecorderConfig.SessionID is required (a dispatch's own session is a throwaway and cannot be the filing key)")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &SubRunRecorder{cfg: cfg}, nil
}

// SubRunRecorder writes one dispatch's mutating intents and completions
// to an IntentStore.
//
// Not safe for concurrent use, and it does not need to be: a recorder
// belongs to one dispatch, and one dispatch is one sub-runner's stream.
type SubRunRecorder struct {
	cfg SubRunRecorderConfig
}

// Observe records the mutating intents an event raises and pairs the
// ones it completes.
//
// # A failed intent write stops the dispatch
//
// The returned error stops the sub-run and hands the planner a labelled
// partial (planner.SubRunSink). That is a stronger posture than the
// other consumers on this seam take — a metering hiccup is swallowed —
// and it is deliberate for three reasons. This path is the one #235
// exists about, and it was judged fail-open enough to refuse a startup
// over. The stop is the mildest one available: one dispatch, not the
// session, and the planner can route around it. And under
// `on_mutation: apply` there is no gate at all, so the record is the
// only control this call has; a control that fails open is not one.
//
// A failed COMPLETION write is logged and swallowed, because by then the
// call has happened and refusing cannot un-happen it. What it leaves
// behind is an intent that stays dangling, which errs toward
// ambiguous-effect mode — the safe direction, and one an operator ack
// clears.
//
// # Why recording is the last thing this seam does
//
// cmd/mast's sink observes metrics first, on the rule that the call
// which crossed a ceiling still cost money. Recording inverts that rule
// on purpose: an intent record is a claim that a mutating call is ABOUT
// to happen, and every consumer ahead of it can still stop the sub-run
// before it does. Recording first would leave a dangling intent for a
// call a watchdog trip or a budget ceiling prevented — a session wedged
// into ambiguous-effect mode over a mutation that never occurred.
func (r *SubRunRecorder) Observe(ev *session.Event) error {
	raised, completed := MutatingCalls(ev, r.cfg.Predicate, r.cfg.SubAgentNames)
	if len(completed) > 0 {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), subRunRecordTimeout)
		err := r.cfg.Store.CompleteSubRunIntents(ctx, r.cfg.SessionID, completed)
		cancel()
		if err != nil {
			r.cfg.Logger.Error("could not pair a dispatched specialist's completed effect; it stays dangling and the next turn will refuse mutating calls until an operator acks",
				"session", r.cfg.SessionID, "specialist", r.cfg.Specialist,
				"calls", completed, "error", err.Error())
		}
	}
	if len(raised) == 0 {
		return nil
	}
	// Detached from the dispatch's context on purpose. The cancellation
	// most likely to arrive mid-dispatch is the one this record is most
	// needed for — a shutdown, a watchdog halt, an operator abort — and a
	// write that dies with the run it is recording records nothing.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), subRunRecordTimeout)
	defer cancel()
	if err := r.cfg.Store.RecordSubRunIntents(ctx, r.cfg.SessionID, r.cfg.Specialist, raised); err != nil {
		r.cfg.Logger.Error("stopping a planner dispatch: its mutating call could not be recorded, and an unrecorded mutation is the hole this seam exists to close",
			"session", r.cfg.SessionID, "specialist", r.cfg.Specialist,
			"calls", describe(raised), "error", err.Error())
		return fmt.Errorf("effects: recording %s's intent to call %s could not be made durable, so the call was not made: %w",
			r.cfg.Specialist, describe(raised), err)
	}
	return nil
}

// Close implements the sink's end-of-dispatch hook. Nothing to flush:
// every record is written as its event arrives, because the failure this
// exists for is the one where Close never runs.
func (r *SubRunRecorder) Close() {}

// MutatingCalls splits one event into the mutating- or spawning-class
// FunctionCalls it raises and the call IDs its FunctionResponses
// complete.
//
// The exclusions are pairScan's, for the same reasons and so the two
// cannot drift: an empty call ID cannot be keyed or paired, a
// long-running park is a pause rather than an effect, engine control
// calls are control flow, and a FunctionCall named after a composed
// agent is a task delegation.
//
// Unlike pairScan this reads ONE event with no history behind it, which
// is all a sink ever has. Pairing across events is the store's job.
func MutatingCalls(ev *session.Event, pred Predicate, subAgents map[string]bool) (raised []DanglingIntent, completed []string) {
	if ev == nil || ev.Content == nil || pred == nil {
		return nil, nil
	}
	longRunning := make(map[string]bool, len(ev.LongRunningToolIDs))
	for _, id := range ev.LongRunningToolIDs {
		longRunning[id] = true
	}
	for _, part := range ev.Content.Parts {
		if part == nil {
			continue
		}
		if fc := part.FunctionCall; fc != nil && fc.ID != "" {
			if longRunning[fc.ID] || controlCalls[fc.Name] || subAgents[fc.Name] {
				continue
			}
			if c := pred(fc.Name); c != ClassMutating && c != ClassSpawning {
				continue
			}
			raised = append(raised, DanglingIntent{
				ToolName:     fc.Name,
				CallID:       fc.ID,
				InvocationID: ev.InvocationID,
				Timestamp:    ev.Timestamp,
				// Not from this session's log, so there is no index into
				// it. -1 says so rather than pointing at an unrelated
				// event: the auto-resume repair path groups by this, and a
				// plausible-looking 0 would put an external intent in the
				// same bucket as the log's first call event.
				EventIndex: -1,
			})
		}
		if fr := part.FunctionResponse; fr != nil && fr.ID != "" {
			// A confirmation-gated call's placeholder response is a pause,
			// not a completion — the same carve-out pairScan makes. It
			// cannot arise inside a dispatch today (the write gate is not
			// installed on the sub-runner, which is the whole premise), so
			// this is here to stay correct if that ever changes.
			if _, pending := ev.Actions.RequestedToolConfirmations[fr.ID]; pending {
				continue
			}
			if fr.Name == toolconfirmation.FunctionCallName {
				continue
			}
			completed = append(completed, fr.ID)
		}
	}
	return raised, completed
}

// WithExternal folds intents recorded outside this session's event log
// into a scan — the dispatched mutations of #235, which by construction
// are absent from the log ScanDangling read.
//
// They join Mutating unconditionally, never Repairable: an external
// intent is only ever recorded for a mutating- or spawning-class call,
// and the repair path must not synthesize a response for one. Synthetic
// answers are also structurally impossible here — the call was made by a
// model in a sub-session that no longer exists, so there is nothing left
// to answer.
func (s DanglingScan) WithExternal(external []DanglingIntent) DanglingScan {
	if len(external) == 0 {
		return s
	}
	out := s
	out.Mutating = append(append([]DanglingIntent(nil), s.Mutating...), external...)
	sortByTimestamp(out.Mutating)
	return out
}
