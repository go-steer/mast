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

// Originally derived from go-steer/core-agent@f93d675d and @eae797f8,
// with the correction at @4095b15a. The mechanism is theirs; the siting
// is not — see the comment on refusals for why none of the three could
// go where upstream put it.

package approval

import (
	"context"
	"fmt"
	"sync"
)

// suppressedCallsEndTurn is how many refused-and-re-proposed calls a
// turn gets before the turn itself ends.
//
// Three, not one: the first re-proposal may be the model reacting to the
// refusal text before it has read it, and ending a turn on a single
// repeat would cut off a specialist that was about to report the refusal
// correctly. Three consecutive suppressions is no longer a model
// finishing its thought; it is a model that will not stop.
//
// It is also deliberately below the watchdog's DefaultRepeatThreshold of
// 5. Both mechanisms watch the same behaviour and only one of them
// should be the one that fires: the watchdog halts the **session** and
// makes an operator reset a guardrail, which is the wrong price for a
// loop the gate already knows about and has already stopped. See
// Config.ForgetToolRun.
const suppressedCallsEndTurn = 3

// refusals is the write gate's memory of what an operator has already
// said no to, for exactly as long as that answer is evidence about the
// call in front of it.
//
// # Why this is not on permissions.Gate
//
// The obvious home is beside sessionAllow / sessionAllowTools /
// sessionAllowVerbs, which is where upstream put the equivalent and
// where the gate keeps every other per-session memory. Two things rule
// it out.
//
// The gate's methods take a context.Context and have no turn identity —
// CheckMutatingToolCall and RecordMutationVerdict cannot tell one turn
// from the next. This memory's entire correctness is the turn boundary:
// a refusal that outlives its turn silently blocks a call the operator
// would have approved, with no symptom. Putting the state somewhere that
// cannot see the boundary means passing the boundary in as a parameter
// on every call, which is the shape that lets one caller pass the wrong
// one.
//
// And permissions.Gate documents, as a property rather than an
// accident, that nothing about a mutation is remembered on any path —
// the reasoning being that a remembered yes for patch_resource hands
// over the namespace. A remembered *no* is the safe direction and does
// not violate the spirit, but it does turn one flat sentence into a
// conditional one, in the file where the flat sentence is the point.
//
// So the memory lives on the plugin that raises the park, and the gate
// stays the stateless policy oracle it says it is.
//
// # The turn boundary is the invocation ID, measured
//
// mast has no pre-turn hook to clear this at, and a parked turn is
// resumed rather than re-driven, so "the turn" needed pinning down
// rather than assuming. What ADK actually does, measured through the
// seam probe and pinned by TestTheTurnBoundaryIsTheInvocationID:
//
//   - the park, the operator's verdict and everything the model does
//     after being told the answer all carry ONE invocation ID, even
//     though the verdict arrives on a second, separate Runner.Run; and
//   - a fresh operator turn gets a new one.
//
// Which is exactly the boundary this needs, so there is no clearing
// step. An entry keyed by a spent invocation ID can never match again.
// forget is called when a run ends, and it is memory hygiene only: a
// forget that never happens leaks a map entry and cannot produce a wrong
// answer. That asymmetry is the reason to key it this way rather than to
// clear it on a hook and trust the hook.
type refusals struct {
	mu sync.Mutex
	// byInvocation is the live turns' memories. A daemon serves many
	// sessions concurrently, and invocation IDs are unique across all
	// of them, so this needs no session awareness of its own.
	byInvocation map[string]*turnRefusals
}

// turnRefusals is one turn's worth: what was refused, and how many times
// the model has proposed a refused call since.
type turnRefusals struct {
	// refused holds the CallKey of every call an operator rejected in
	// this turn.
	refused map[string]struct{}
	// suppressed counts re-proposals this gate has turned away. It is a
	// turn-level count and not a per-call one on purpose: a model
	// cycling through three refused calls in rotation is the same
	// failure as one model repeating one call three times, and only the
	// turn-level count catches it.
	suppressed int
}

func newRefusals() *refusals {
	return &refusals{byInvocation: map[string]*turnRefusals{}}
}

// remember records that an operator refused key in the turn identified
// by invocationID. Only a refusal reaches here; see the call site in
// honorVerdict for the three near-misses that deliberately do not.
func (r *refusals) remember(invocationID, key string) {
	if invocationID == "" {
		// No turn identity means no boundary to expire at, and a
		// suppression that cannot expire is worse than none. mast's own
		// runner always supplies one; a library embed driving the
		// callback by hand might not.
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.byInvocation[invocationID]
	if t == nil {
		t = &turnRefusals{refused: map[string]struct{}{}}
		r.byInvocation[invocationID] = t
	}
	t.refused[key] = struct{}{}
}

// suppress reports whether key was already refused in this turn and, if
// so, counts the attempt. endTurn is true once the count reaches
// suppressedCallsEndTurn.
//
// The count advances only on a suppressed call, so a model that reacts
// to a refusal by doing something else entirely — the behaviour the
// refusal text asks for — never approaches it however long it works.
func (r *refusals) suppress(invocationID, key string) (suppressed, endTurn bool, count int) {
	if invocationID == "" {
		return false, false, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.byInvocation[invocationID]
	if t == nil {
		return false, false, 0
	}
	if _, ok := t.refused[key]; !ok {
		return false, false, 0
	}
	t.suppressed++
	return true, t.suppressed >= suppressedCallsEndTurn, t.suppressed
}

// forget drops a finished turn's memory. Hygiene, not correctness: see
// the type comment.
func (r *refusals) forget(invocationID string) {
	if invocationID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byInvocation, invocationID)
}

// TurnStop ends the turn the gate is running in.
//
// # Why this is not just returning an error
//
// It is the natural shape and it does not work. ADK's callTool turns any
// error a before-tool callback returns into map[string]any{"error":
// err.Error()} — a tool response like any other — and the flow carries
// on to the next model call. Measured, not assumed: the first version of
// TestThreeSuppressedCallsEndTheTurn watched the gate report suppression
// 4 to a run that was supposed to have ended at 3, with nothing on the
// runner's error channel. A refusal the model can simply read and ignore
// is the thing #449 exists to stop being.
//
// So the turn ends the one way a mast turn ends: the host cancels the
// run context. That is what the watchdog does, what a budget trip does
// and what an operator abort does, and it carries the same obligation
// the watchdog's halt carries — a cancelled run surfaces as "context
// canceled", so the host has to keep the reason somewhere it can report
// it from afterwards (see pkg/watchdog's Preflight, and cmd/mast's
// refusalStop).
//
// # Why it rides the context
//
// Same reason WithCallGate does. A planner-dispatched specialist runs on
// a private sub-runner with a session ID of its own, so a stop the host
// looked up by session would miss exactly the agent most likely to be in
// a loop. The context reaches it.
//
// A composition that installs none is not broken: nothing ends the turn,
// and the gate goes on refusing every re-proposal for as long as the
// model keeps making them. That is still strictly better than parking
// each one, and it is the right default for a library embed that owns
// its own loop and would not want a dependency cancelling it.
type TurnStop interface {
	// StopTurn ends the turn, recording reason as what ended it. It may
	// be called more than once in a turn; the first reason is the one
	// that counts.
	StopTurn(reason error)
}

type turnStopKey struct{}

// WithTurnStop returns ctx carrying stop, for the write gate to find.
// Call it once where a turn starts, on the context handed to
// runner.Run; everything below inherits it.
//
// A nil stop returns ctx unchanged rather than installing an absence.
func WithTurnStop(ctx context.Context, stop TurnStop) context.Context {
	if stop == nil {
		return ctx
	}
	return context.WithValue(ctx, turnStopKey{}, stop)
}

// TurnStopFrom returns the stop on ctx, or nil if this turn cannot be
// ended from inside.
func TurnStopFrom(ctx context.Context) TurnStop {
	stop, _ := ctx.Value(turnStopKey{}).(TurnStop)
	return stop
}

// RefusalLoopError is the reason a turn ended in which the model kept
// re-proposing a call an operator had already refused.
//
// It is a turn error and not a session halt, which is the whole
// distinction #449 turns on. The watchdog would have caught this
// eventually — a run of identical calls is exactly what it watches — but
// it halts the session and makes an operator clear a guardrail before
// the daemon works again. One "no" should not cost that. So the gate
// stops the turn on its own evidence, at a threshold below the
// watchdog's, and the next turn starts clean with nothing to reset.
//
// The error names the tool and the call so an operator reading a
// one-line turn failure knows which refusal the model would not accept.
type RefusalLoopError struct {
	Tool  string
	Key   string
	Count int
}

func (e *RefusalLoopError) Error() string {
	return fmt.Sprintf(
		"turn ended: the model re-proposed a call an operator refused %d times (tool=%s call=%q). "+
			"Nothing was executed and no approval is pending. The refusal stands; the next turn starts clean.",
		e.Count, e.Tool, e.Key)
}

// TurnErrorKind implements the attach package's SelfClassifyingError
// without importing it — see the interface's own comment for why the
// contract is a bare string. The value is pinned against
// attach.TurnErrorRefusalLoop in this package's tests.
func (e *RefusalLoopError) TurnErrorKind() string { return "refusal_loop" }
