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

package approval

import (
	"context"
	"time"

	"google.golang.org/adk/v2/agent"
)

// The out-of-band half of a park (#451).
//
// A park is durable and discoverable, and every way of discovering it is
// a PULL: GET /parks, the attach stream, `mast sessions show`. An
// unattended daemon on a fifteen-minute cadence can park at 03:00 and
// sit there until somebody happens to look. The gate can be operated
// only if raising a park can also PUSH.
//
// The shape is core-agent's, from a0bcfe66 (their #647/#1059), with one
// deliberate divergence recorded in docs/sibling-sync.md. Upstream fires
// only when the prompt's fan-out reached nobody — it counts DELIVERIES
// rather than subscribers, because a subscriber whose buffer is full is
// one whose reader stopped draining. That measurement is right for a
// prompt that is a blocked goroutine with a deadline: whoever is
// attached at the moment it opens is whoever can answer it before it
// expires.
//
// mast's park is neither. It is a durable row that outlives the process,
// so "somebody was attached when it was raised" says nothing about
// whether anybody is attached an hour later, and an operator who
// detaches thirty seconds after a park opens would get silence for the
// rest of the night. So mast announces every park it is configured to
// announce, and the cost of that choice is a redundant message to an
// operator who was already watching. That is the cheap direction; the
// expensive one is the park nobody hears about.
//
// Announcing a park only once it has gone UNANSWERED for some interval
// would be better than either, and is a different change: it needs a
// durable timer rather than a goroutine, which is the same machinery the
// deferred approval-timeout question needs. See #451.

// notifyTimeout bounds one out-of-band announcement. Generous next to
// the egress client's own 30s HTTP timeout because this budget also
// covers DNS and connection setup on a daemon that may not have spoken
// to the destination since it started.
const notifyTimeout = 60 * time.Second

// ParkNotice is what the gate hands NotifyPark when it parks a call: the
// facts an operator woken by the announcement needs in order to find the
// park and answer it, and nothing the model wrote.
//
// The raw argument map is deliberately absent, but do not read that as
// "the notice discloses nothing about the arguments". Key and Hint are
// the operator-facing rendering of the call, and CallKey renders
// argument VALUES into it, elided at 120 characters each —
// `scale_deployment(deployment=api, replicas=10)`. A notice therefore
// puts a truncated view of a mutating call's arguments wherever it is
// sent, and a destination whose readership is not the session's ACL is
// choosing that. What a notice never carries is the untruncated payload:
// the full arguments stay in the confirmation record, which
// `mast sessions show` and GET /parks serve to somebody who already has
// the session.
type ParkNotice struct {
	// Session is the session the park belongs to, and the one an
	// operator needs to answer it.
	Session string

	// Invocation is the turn the park was raised in. Two parks in one
	// turn share it, and so do a park and the resumed call that carries
	// the operator's answer — one invocation ID spans the whole of it.
	Invocation string

	// Workload names the bundle that composed the gate, empty for a
	// library embed that registered this plugin itself.
	Workload string

	// Agent is the agent that proposed the call. A specialist's park and
	// the coordinator's read identically without it, and they are not
	// equally surprising.
	Agent string

	// Tool is the tool that would run, and Key its call key — the same
	// string the audit log, the decision record and GET /parks use.
	Tool string
	Key  string

	// Hint is the one line an operator sees first, already carrying the
	// change-set and stale-grant context when there is any. See
	// parkHint.
	Hint string

	// ParkedAt is when the park was raised, in UTC.
	ParkedAt time.Time
}

// announcePark hands the notice to the configured notifier on its own
// goroutine, with a context that does NOT inherit the turn's
// cancellation — only a bound of its own.
//
// The detachment is the point rather than a convenience. A park is the
// end of the turn's useful work — the gate hands the model
// "awaiting_operator_approval" and tells it to stop — so the turn's
// context is at its closest to being cancelled at exactly the moment the
// announcement is most worth sending. The turn can also be cut by its
// own wallclock budget, by a SIGTERM drain, or by the watchdog, and in
// each of those the park survives while the context does not. A send
// parented on it would be killed by the very shutdown that makes the
// announcement matter.
//
// The gate does not wait for the send and never sees its error. A park
// must stay answerable while its announcement is failing, so the gate is
// not made to depend on a webhook — a notifier that wants its failures
// seen logs them itself, which is what cmd/mast's does.
func (g *writeGate) announcePark(ctx agent.Context, notice ParkNotice) {
	fn := g.cfg.NotifyPark
	if fn == nil {
		return
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	go func() {
		defer cancel()
		fn(nctx, notice)
	}()
}
