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

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/notify"
	"github.com/go-steer/mast/pkg/observability"
)

// The push half of a durable park (#451).
//
// mast can park a turn for an operator decision and tell nobody. The
// park itself is fine — durable, discoverable, answerable hours later —
// but GET /parks, the attach stream and `mast sessions show` are all
// PULLS, and an unattended workload on a fifteen-minute cadence can park
// at 03:00 and sit there until somebody happens to look. This file is
// the door out: one chat message per park, through the same switchboard
// ingress a monitoring cycle speaks to.
//
// Four properties are load-bearing, and three of them are core-agent's
// (a0bcfe66, their #647/#1059) rather than invented here.
//
// THE DESTINATION IS A DAEMON FACT. --park-notify names a conversation
// and the URL and bearer are already deployment configuration, so
// nothing in a workload bundle and nothing a model emits can choose
// where a park announcement goes. A bundle-chosen destination would put
// the routing of "mast is asking permission" inside the thing being
// asked about.
//
// THE SENDER IS NOT GATED. It posts on mast's own behalf, not the
// model's: a gate asking permission to report that it needs permission
// is a circle with nobody attached to break it.
//
// THE BUDGET IS THIS FILE'S, NOT THE WORKLOAD'S. The monitoring egress
// and this one share a client and share nothing else — no timeline, no
// deadman, no message being extended. A park notice is always its own
// message, never an append, so a park raised mid-incident cannot splice
// itself into the story a monitoring cycle is telling, and a monitoring
// cycle cannot consume the budget that gets a park announced. The bucket
// below is the second half of that: a model that parks in a loop can
// exhaust its own announcements and nothing else, and the exhaustion is
// counted rather than silent.
//
// A FAILURE IS THE DAEMON'S, NOT THE TURN'S. Nothing here returns an
// error to the gate. A park must stay answerable while its announcement
// is failing, so a failed send is an ERROR log line plus
// mast_park_notifications_total{outcome="error"} — "we tried to tell you
// and could not" is the most useful line in the log of a run that
// stalled — and the turn is never told.

// The park announcement budget. Three in hand, one back every five
// minutes.
//
// Sized for what it is defending against rather than for throughput. A
// healthy gated workload parks once and stops, so the burst covers the
// genuinely bursty cases — a graph fan-out whose branches each propose a
// change, a redeploy that resumes several interrupted sessions at once —
// while the refill puts a ceiling of twelve an hour on a workload that
// has started parking in a loop. Twelve an hour is already past the
// point where an operator mutes the channel, which is the outcome worth
// preventing: a muted channel costs them the park that mattered too.
const (
	parkNotifyBurst  = 3
	parkNotifyRefill = 5 * time.Minute
)

// parkNotifier announces parks to one conversation. Nil-safe, so the
// gate wiring holds one whether or not an operator configured it.
type parkNotifier struct {
	client   notifySender
	conv     string
	workload string
	logger   *slog.Logger
	obs      *observability.Registry
	now      func() time.Time

	// mu guards the bucket. Parks arrive from whichever turn raised
	// them, and a multi-session daemon runs several at once.
	mu sync.Mutex
	// tokens is what is left of the burst; refilledAt is when the last
	// token was granted. Held as a count plus a timestamp rather than a
	// ticker so an idle daemon costs no goroutine and a daemon that
	// slept through a suspend wakes up with a correctly refilled bucket.
	tokens     int
	refilledAt time.Time
	// throttling is the edge tracker for the log line. A workload
	// parking in a loop says so once; the counter carries the rate.
	throttling bool
}

// buildParkNotifier constructs the park egress, or returns nil for a
// daemon that was not asked for one.
//
// The conversation is a daemon flag and the credential is an
// environment variable, which is the same split --notify-url already
// makes and for the same reason: which switchboard and which channel are
// deployment facts, and a bundle that named them could not be moved
// between staging and production without editing the workload.
//
// A configured conversation with no ingress is a startup error rather
// than a warning. An operator who sets --park-notify has told us they
// are not watching a console, so degrading to a console warning hands
// them exactly the silence they configured against — the same reasoning
// buildNotifyClient already applies to a missing MAST_NOTIFY_TOKEN.
func buildParkNotifier(logger *slog.Logger, obs *observability.Registry, workloadName, conv string, client notifySender) (*parkNotifier, error) {
	if err := parkNotifyConfigError(conv, client); err != nil {
		return nil, err
	}
	conv = strings.TrimSpace(conv)
	if conv == "" {
		return nil, nil
	}
	now := func() time.Time { return time.Now().UTC() }
	logger.Info("approval parks will be announced to the chat ingress",
		"conversation", conv, "burst", parkNotifyBurst, "refill", parkNotifyRefill)
	return &parkNotifier{
		client:     client,
		conv:       conv,
		workload:   workloadName,
		logger:     logger,
		obs:        obs,
		now:        now,
		tokens:     parkNotifyBurst,
		refilledAt: now(),
	}, nil
}

// parkNotifyConfigError is the one configuration error --park-notify
// can have. Extracted so the startup refusal and the constructor cannot
// drift: serve refuses before it binds a listener, which is well before
// the metric registry the notifier itself needs exists.
func parkNotifyConfigError(conv string, client notifySender) error {
	if strings.TrimSpace(conv) == "" || client != nil {
		return nil
	}
	return fmt.Errorf("--park-notify is set to %q but no chat ingress is configured; set --notify-url and %s", strings.TrimSpace(conv), notifyTokenEnv)
}

// announce is the approval.Config.NotifyPark hook. The gate calls it on
// its own goroutine with a detached, separately bounded context, so this
// runs to completion even though the turn that parked is over.
//
// Nil-safe: a daemon with no park egress wires this method anyway and it
// does nothing, which keeps one wiring path instead of two.
func (p *parkNotifier) announce(ctx context.Context, n approval.ParkNotice) {
	if p == nil || p.client == nil {
		return
	}
	if !p.take() {
		p.obs.ParkNotify(p.workload, observability.ParkNotifyThrottled)
		p.logger.Warn("a park was not announced: this workload is parking faster than its announcement budget allows; the park is still recorded and still answerable",
			"session", n.Session, "tool", n.Tool, "call", n.Key,
			"conversation", p.conv, "budget", fmt.Sprintf("%d then 1 per %s", parkNotifyBurst, parkNotifyRefill))
		return
	}
	if _, err := p.client.Post(ctx, p.conv, parkNotifyText(n), parkNotifyIdem(n)); err != nil {
		p.obs.ParkNotify(p.workload, observability.ParkNotifyError)
		// ERROR, and it names what was lost rather than just what
		// failed: the operator reading this later needs to know a
		// decision was waiting on them, not that an HTTP call returned
		// a 503.
		p.logger.Error("could not tell the chat ingress that a call is parked; nobody was notified that this daemon is waiting for an approval",
			"session", n.Session, "tool", n.Tool, "call", n.Key,
			"conversation", p.conv, "error", err.Error(), "denied", errors.Is(err, notify.ErrDenied))
		return
	}
	p.obs.ParkNotify(p.workload, observability.ParkNotifySent)
	p.logger.Info("told the chat ingress that a call is parked",
		"session", n.Session, "tool", n.Tool, "call", n.Key, "conversation", p.conv)
}

// take spends one token, refilling first. Reports whether there was one
// to spend.
func (p *parkNotifier) take() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if gained := int(now.Sub(p.refilledAt) / parkNotifyRefill); gained > 0 {
		p.tokens = min(p.tokens+gained, parkNotifyBurst)
		// Advanced by whole refill periods rather than set to now, so
		// the remainder carries and a steady drip does not lose time to
		// rounding on every call.
		p.refilledAt = p.refilledAt.Add(time.Duration(gained) * parkNotifyRefill)
	}
	if p.tokens <= 0 {
		p.throttling = true
		return false
	}
	p.tokens--
	if p.throttling {
		p.throttling = false
		p.logger.Info("park announcements have budget again", "conversation", p.conv)
	}
	return true
}

// parkNotifyText is the message. mast wrote it, no model did, and it
// says so: an operator woken at 3am should be able to tell a report the
// agent produced from the daemon reporting on itself.
//
// WHAT IT DISCLOSES, stated plainly, because a chat channel's readership
// is not a session's ACL. The hint is the operator-facing rendering of
// the call, and that rendering includes argument VALUES, elided at 120
// characters each — `scale_deployment(deployment=api, replicas=10)`. So
// announcing a park puts a truncated view of a mutating call's arguments
// into the configured conversation, and an operator who sets
// --park-notify is choosing that. What does not travel is the
// untruncated payload: the full arguments stay in the confirmation
// record that `mast sessions show` and GET /parks serve.
//
// Dropping the hint to avoid even that was the alternative, and it is
// worse. The hint is the only part of the notice that says which call is
// waiting and whether it is one of a set, so a notice without it pages
// an operator to go and find out what they were paged about — which is
// most of the latency this feature exists to remove.
func parkNotifyText(n approval.ParkNotice) string {
	var b strings.Builder
	b.WriteString("mast stopped to ask for an approval and is waiting. Nothing has changed.\n\n")
	if n.Workload != "" {
		fmt.Fprintf(&b, "Workload: %s\n", n.Workload)
	}
	fmt.Fprintf(&b, "Session:  %s\n", n.Session)
	if n.Agent != "" {
		fmt.Fprintf(&b, "Agent:    %s\n", n.Agent)
	}
	// The bare tool name, scannable; the hint below carries the full call
	// key, so a separate Call: line would print the arguments twice.
	fmt.Fprintf(&b, "Tool:     %s\n", n.Tool)
	fmt.Fprintf(&b, "Parked:   %s\n", n.ParkedAt.UTC().Format(time.RFC3339))
	if n.Hint != "" {
		fmt.Fprintf(&b, "\n%s\n", n.Hint)
	}
	// The instruction, not just the facts. The interrupt ID is minted
	// with the park and is not known here, so the first command is the
	// one that prints it — and it prints the exact resume line too.
	fmt.Fprintf(&b, "\nThe call has NOT been made and will not be until somebody answers:\n\n  mast sessions show %s\n\n", n.Session)
	b.WriteString("That prints the pending interrupt and the `mast sessions resume` line to answer it; " +
		"this daemon's GET /parks serves the same park over HTTP. " +
		"This notice is sent once, when the park opens, and is not repeated.")
	return b.String()
}

// parkNotifyIdem is this park's replay key, so a send that timed out
// client-side after landing is not double-posted.
//
// The invocation plus a digest of the call key: two parks in one turn
// differ by key, the same call parked in a later turn differs by
// invocation, and hashing keeps the key bounded regardless of how large
// the arguments the call key covers were.
func parkNotifyIdem(n approval.ParkNotice) string {
	sum := sha256.Sum256([]byte(n.Key))
	return fmt.Sprintf("mast:park:%s:%s", n.Invocation, hex.EncodeToString(sum[:8]))
}
