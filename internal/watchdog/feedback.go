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

// Originally derived from go-steer/core-agent@6510a65b54ead93b5f2c8c31f478443376203360:pkg/agent/watchdog.go

// Feedback mode: routing the observation to the party that can act on
// it.
//
// Every posture before this one told an operator. On an unattended
// workload — mast's whole premise — that operator is a pod log nobody
// is tailing, and even a watching one can only interrupt a turn already
// in flight. The model choosing the next tool call is the only party
// that can stop making it, and it was the one party never told.
//
// Upstream queues the pending alerts on its Agent struct and prepends
// them inside Run. mast has no Agent, so the queue is a value the
// caller holds per session, symmetric with Enforcer: the package
// decides *what* the model should be told and when it has been told it;
// the host decides how a prompt is assembled.

package watchdog

import (
	"strings"
	"sync"
)

// FeedbackHeader opens the block FormatFeedback renders. Exported so
// hosts, tests and transcript tooling can find the boundary without
// re-typing the literal.
const FeedbackHeader = "[watchdog]"

// FormatFeedback renders alerts as the model-facing block that
// --watchdog=feedback prepends to the next turn's prompt. Returns ""
// for an empty slice so callers can skip the prepend cheaply.
//
// Two things the wording has to do. It must be unmistakably about the
// model's *own last turn* rather than a message from the user — a model
// that reads "you called kubectl_get 5 times" as a user complaint will
// apologize instead of changing behavior. And it must carry an
// instruction rather than a description: a description of the loop is
// precisely what the model already had.
//
// Not a trust boundary. A user prompt can contain the literal
// "[watchdog]" string exactly as it can contain any other marker; this
// block is steering, and nothing downstream grants authority based on
// it.
func FormatFeedback(alerts []Alert) string {
	if len(alerts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(FeedbackHeader)
	b.WriteString(" Automated observation about your own previous turn — this is not a message from the user, and the user cannot see it.\n")
	for _, a := range alerts {
		text := a.Guidance
		if text == "" {
			// A third-party signal with no model-facing half. Reason is
			// worse — it may name operator controls the model cannot
			// use — but it is the observation, and dropping the alert
			// would make a custom signal silently inert under feedback
			// mode.
			text = a.Reason
		}
		b.WriteString("- ")
		b.WriteString(a.Signal)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	b.WriteString("Adjust your approach on this turn accordingly.")
	return b.String()
}

// MaxPendingFeedback caps the queue of alerts awaiting injection. The
// queue drains on every turn, so it only grows when a host observes
// turns without starting new ones; the bound keeps that case from
// becoming an ever-growing prompt prefix. The oldest are dropped,
// because the newest observation describes the behavior the model is
// about to repeat.
const MaxPendingFeedback = 4

// Feedback holds one session's queue of observations awaiting delivery
// to the model.
//
// One per session, alongside the Watchdog and the Enforcer: an
// observation about one session's loop is meaningless prepended to
// another's prompt.
//
// Safe for concurrent use. Alerts arrive from the event tap while the
// turn path may be draining the queue.
type Feedback struct {
	mu      sync.Mutex
	mode    Mode
	pending []Alert
}

// NewFeedback returns a queue in the given posture. The zero Mode is
// ModeWarn, which queues nothing.
//
// The mode lives here rather than at the call site so the "queue
// nothing below feedback" rule holds for every caller: were a host to
// queue under warn and only gate the injection, flipping a long-running
// deployment to feedback would deliver a backlog of observations about
// turns that ended hours ago.
func NewFeedback(mode Mode) *Feedback {
	if mode == "" {
		mode = ModeWarn
	}
	return &Feedback{mode: mode}
}

// Queue records alerts for delivery on the session's next turn. A no-op
// below ModeFeedback, and a no-op for an empty slice.
func (f *Feedback) Queue(alerts []Alert) {
	if f == nil || len(alerts) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.mode.Feeds() {
		return
	}
	f.pending = append(f.pending, alerts...)
	if n := len(f.pending) - MaxPendingFeedback; n > 0 {
		f.pending = f.pending[n:]
	}
}

// Drain returns the queued alerts and empties the queue. Returns nil
// when nothing is pending.
//
// Draining on read rather than on turn success means an observation is
// delivered exactly once even if the turn it lands in fails. Losing it
// in that case is the right trade: by the time a retry lands the signal
// describes behavior several turns back, and a block that re-appears
// every turn until some turn succeeds is a prompt leak.
func (f *Feedback) Drain() []Alert {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil
	}
	out := f.pending
	f.pending = nil
	return out
}

// Pending reports how many observations are waiting. For tests and for
// the guardrail projection; the queue is not otherwise introspectable.
func (f *Feedback) Pending() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}
