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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/mast/pkg/monitor"
	"github.com/go-steer/mast/pkg/notify"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/workload"
)

// The egress leg (v0.5 W4.5): the part of a monitoring cycle that
// decides whether to say anything, and to what.
//
// W4.2 made a cycle gather its own facts and W4.4 made it read the
// classification without owning it. Both were in service of the
// decision here, which is the one an unattended workload has to make on
// its own at 3am: SPEAK ONLY WHEN SOMETHING CHANGED.
//
// Three rules, in the order they are applied.
//
// A CYCLE WHOSE CLASSIFICATION IS EMPTY DOES NOT WAKE THE MODEL AT ALL.
// Not "wakes it and then declines to post" — the turn never runs. This
// is the strongest form of the claim and the only one worth making: a
// fifteen-minute cadence that spends a model call on every quiet cycle
// costs more per month in nothing-happened than the incidents it exists
// to catch. It is also the narrowest form. mast skips the turn only
// when the workload named a `monitor.transitions_from` (so "nothing
// changed" is the classifier's answer, not mast's guess) AND declared a
// `monitor.notify` block (so speaking is what the cycle is for). A
// workload that does neither is unaffected: it runs every tick, as it
// always did, and its assessment lands in the transcript.
//
// CONSECUTIVE SPEAKING CYCLES EXTEND ONE MESSAGE. An incident that
// takes six cycles to resolve should read as one growing story in the
// channel, not six notifications an operator has to reassemble. So the
// first speaking cycle posts and the next ones append to it, and the
// first quiet cycle CLOSES the timeline — the next thing to happen
// after calm is a new incident, and it gets a new message. The ingress
// can answer an append with "I no longer remember that message" or with
// "that message is full, here is its continuation", and both are
// handled rather than surfaced: see pkg/notify.
//
// SILENCE IS BOUNDED BY A DEADMAN, NOT BY A COUNT. `digest_after` is
// wall-clock: after that long without saying anything, the next cycle
// speaks whether or not it has news. A monitor that has been quiet for
// a week is indistinguishable from a monitor that died a week ago, and
// the whole value of the feature is that somebody trusts the silence.
//
// # What is NOT here
//
// No queue, no retry, no spool. A failed send is an errored fire and
// nothing more. It is tempting to hold the assessment and re-send it
// next cycle, and it is wrong: the state that produced it already
// advanced during collection (the classifier consumed the diff), so a
// replay would describe a world that has moved on. The next cycle
// reports what is new THEN, which is the honest answer, and the failed
// send is visible as mast_monitor_notifications_total{outcome="error"}.

// timelineSep separates one cycle's assessment from the previous one
// inside a message that several cycles have extended. A blank line,
// because every chat platform switchboard bridges renders one.
const timelineSep = "\n\n"

// Idempotency-key suffixes. Every request a cycle makes carries a key
// derived from the tick, so a send that timed out client-side after
// landing is not double-posted by the retry above it. The suffixes keep
// the fallbacks distinct: switchboard fingerprints the body against the
// key and answers 409 if one key is reused for a different request, and
// the whole point of a fallback is that it sends something different.
const (
	idemFull   = ":full"
	idemNew    = ":new"
	idemHealth = ":health"
)

// notifyTokenEnv is where the ingress bearer comes from. An env var and
// never a flag: it is a credential, and a flag would put it in every
// `ps` on the node and in the container spec's args.
const notifyTokenEnv = "MAST_NOTIFY_TOKEN" // #nosec G101 -- the name of the variable, not a credential

// buildNotifyClient constructs the chat egress from the daemon's own
// configuration, or returns nil for a daemon that was not given one.
//
// The URL and the token live on the daemon rather than in the bundle
// because they are deployment facts — which switchboard, which
// credential — while the bundle owns the workload facts: which
// conversation, and how long silence may last. A bundle that named its
// own ingress could not be moved between a staging and a production
// deployment without editing the workload.
//
// The token is refused if it matches one of the daemon's own inbound
// credentials. They point in opposite directions: the inbound tokens
// let a caller drive this daemon, and the egress token lets this daemon
// post into a chat. Sharing one means anything that can read the chat
// bridge's configuration can inject turns here, which is not a trade an
// operator would make on purpose, and is very easy to make by accident
// when both are pasted from the same secret.
func buildNotifyClient(logger *slog.Logger, baseURL string, inbound map[string]string) (notifySender, error) {
	baseURL = strings.TrimSpace(baseURL)
	token := strings.TrimSpace(os.Getenv(notifyTokenEnv))
	if baseURL == "" {
		if token != "" {
			logger.Warn("a chat ingress token is set but no ingress URL is; monitoring cycles will not be able to report",
				"token_env", notifyTokenEnv, "url_flag", "--notify-url")
		}
		return nil, nil
	}
	if token == "" {
		return nil, fmt.Errorf("--notify-url is set but %s is not; the ingress always requires a bearer token", notifyTokenEnv)
	}
	for name, other := range inbound {
		if other != "" && other == token {
			return nil, fmt.Errorf("%s is the same value as %s; the token this daemon posts chat with must not also be a token that drives this daemon", notifyTokenEnv, name)
		}
	}
	c, err := notify.New(notify.Config{BaseURL: baseURL, Token: token})
	if err != nil {
		return nil, err
	}
	logger.Info("chat ingress configured for monitoring notices", "endpoint", c.Endpoint())
	return c, nil
}

// notifySender is the slice of pkg/notify.Client this file uses, named
// so the timeline can be tested without an HTTP server standing in for
// a chat platform.
type notifySender interface {
	Post(ctx context.Context, conversation, text, idem string) (notify.Ref, error)
	Replace(ctx context.Context, ref notify.Ref, text, idem string) error
	Append(ctx context.Context, ref notify.Ref, line, idem string) (notify.Ref, error)
}

// cycleSpeech is what one cycle decided to do about talking, decided
// BEFORE the model is woken because it determines whether the model is
// woken at all.
type cycleSpeech int

const (
	// speechUnconfigured: this workload declares no notify block. The
	// cycle runs exactly as it did before W4.5 and posts nothing.
	speechUnconfigured cycleSpeech = iota
	// speechQuiet: the classification came back empty. No model call, no
	// request, and the open timeline is closed.
	speechQuiet
	// speechSpeak: there is something to say.
	speechSpeak
	// speechDigest: nothing changed, but the deadman expired, so the
	// cycle runs and reports the calm.
	speechDigest
)

// speaks reports whether the model is woken and the answer sent.
func (s cycleSpeech) speaks() bool { return s == speechSpeak || s == speechDigest }

// notifier keeps one workload's chat timeline across cycles.
//
// State lives in the process, not in the store, and that is deliberate.
// What is remembered here is "which message am I currently extending",
// and after a restart the honest answer is "none": the daemon cannot
// know whether the message it posted before the crash is still the last
// thing in the channel. A fresh post after a restart is one extra
// message; appending to a stale one is a story with a hole in it.
type notifier struct {
	client   notifySender
	conv     string
	digest   time.Duration
	workload string
	logger   *slog.Logger
	obs      *observability.Registry
	now      func() time.Time

	// mu guards the timeline. The cadence fires one cycle at a time, so
	// this is not contended; it is here because the health notices can
	// be written from a fire that is failing while a drain reads.
	mu sync.Mutex
	// ref is the message being extended, zero when the timeline is
	// closed. text is that message's full body, which is what a 409
	// ("I no longer remember it") asks mast to re-send.
	ref  notify.Ref
	text string
	// spokeAt is when anything was last sent, and the deadman counts
	// from it. Seeded at construction so a daemon that boots into a
	// quiet world does not immediately announce the quiet.
	spokeAt time.Time
	// failing is the edge tracker for the health notices: a monitor that
	// is broken says so once, not every fifteen minutes.
	failing bool
}

// newNotifier builds the egress leg from the bundle, or returns nil for
// a workload that declares no notify block. A client is required when
// the block IS present — the caller has already refused to start
// otherwise, because a bundle that says to speak and a daemon that
// cannot is a monitor an operator believes is reporting.
func newNotifier(logger *slog.Logger, obs *observability.Registry, workloadName string, mon workload.Monitor, client notifySender) (*notifier, error) {
	if mon.Notify == nil {
		return nil, nil
	}
	conv := mon.NotifyTarget()
	if conv == "" {
		return nil, errors.New("monitor.notify names no conversation")
	}
	digest, err := mon.EffectiveDigestAfter()
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("workload posts monitoring notices to %q but no chat ingress is configured; set --notify-url and MAST_NOTIFY_TOKEN", conv)
	}
	now := func() time.Time { return time.Now().UTC() }
	return &notifier{
		client:   client,
		conv:     conv,
		digest:   digest,
		workload: workloadName,
		logger:   logger,
		obs:      obs,
		now:      now,
		spokeAt:  now(),
	}, nil
}

// enabled reports whether this workload speaks at all. Nil-safe, so the
// fire path holds one *notifier whether or not the bundle configured
// one.
func (n *notifier) enabled() bool { return n != nil && n.client != nil && n.conv != "" }

// decide reads the cycle's classification and says what happens next,
// with the reason for the log line.
//
// A nil set is "this workload does not classify", and it speaks every
// cycle: without a transitions source mast has no basis for calling a
// cycle quiet, and inventing one — diffing the collected results
// itself, say — is exactly the domain knowledge pkg/monitor refuses to
// hold. An empty set is the classifier saying nothing changed, which is
// the answer this whole leg is built to act on.
func (n *notifier) decide(t *monitor.Set) (cycleSpeech, string) {
	if !n.enabled() {
		return speechUnconfigured, "no notify block"
	}
	if t == nil {
		return speechSpeak, "the workload classifies nothing, so every cycle reports"
	}
	if !t.Empty() {
		return speechSpeak, fmt.Sprintf("%d transition(s)", len(t.Transitions))
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.digest > 0 && !n.now().Before(n.spokeAt.Add(n.digest)) {
		return speechDigest, fmt.Sprintf("nothing changed, and nothing has been said for %s", n.digest)
	}
	return speechQuiet, "nothing changed"
}

// quiet records a cycle that said nothing and closes the timeline, so
// the next thing that happens gets a message of its own rather than
// being appended to the last incident.
func (n *notifier) quiet(sessionID, reason string) {
	if !n.enabled() {
		return
	}
	n.mu.Lock()
	closed := !n.ref.Zero()
	n.ref, n.text = notify.Ref{}, ""
	n.mu.Unlock()
	n.obs.MonitorNotify(n.workload, observability.MonitorNotifyQuiet)
	// Logged at Info, not Debug: "the cycle ran and decided not to wake
	// anyone" is the single most common thing a healthy monitor does,
	// and an operator asking whether it is still running needs to see it
	// without turning up the verbosity of everything else.
	n.logger.Info("monitoring cycle stayed quiet; no model was woken and nothing was sent",
		"session", sessionID, "conversation", n.conv, "reason", reason, "closed_timeline", closed)
}

// speak sends one cycle's assessment, extending the open timeline or
// opening a new one, and returns an error only when the operator's chat
// did not get the message.
func (n *notifier) speak(ctx context.Context, sessionID, text, idem string) error {
	if !n.enabled() {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		// The cycle had something to report and produced no sentence to
		// report it in — a turn that halted, or paused for an approver.
		// mast does not write one on the model's behalf: a synthetic
		// "something changed" with no content is worse than an errored
		// fire, because it looks like a report.
		n.obs.MonitorNotify(n.workload, observability.MonitorNotifyError)
		return errors.New("the cycle had something to report and the turn produced no text; nothing was sent")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	outcome, err := n.send(ctx, text, idem)
	if err != nil {
		// The timeline is closed on any failure: mast no longer knows
		// what the message in the channel says, and appending to it
		// would build on a body it cannot see.
		n.ref, n.text = notify.Ref{}, ""
		n.obs.MonitorNotify(n.workload, observability.MonitorNotifyError)
		return err
	}
	n.spokeAt = n.now()
	n.obs.MonitorNotify(n.workload, outcome)
	n.logger.Info("monitoring cycle reported to the chat ingress",
		"session", sessionID, "conversation", n.conv, "outcome", outcome, "message", n.ref.ID)
	return nil
}

// send is the timeline state machine. Caller holds mu.
func (n *notifier) send(ctx context.Context, text, idem string) (string, error) {
	if n.ref.Zero() {
		return n.post(ctx, text, idem)
	}
	ref, err := n.client.Append(ctx, n.ref, text, idem)
	if err == nil {
		if ref != n.ref {
			// Rolled over into a continuation. The old message keeps
			// what it had; this one starts from what was just said, and
			// every later append targets it.
			n.ref, n.text = ref, text
			return observability.MonitorNotifyRolled, nil
		}
		n.text += timelineSep + text
		return observability.MonitorNotifyAppended, nil
	}
	if !errors.Is(err, notify.ErrSendFullText) && !errors.Is(err, notify.ErrNoSuchMessage) {
		return "", err
	}
	// The ingress cannot extend the message mast is holding. Everything
	// the timeline has said, plus this cycle, sent whole — the operator
	// reading the channel should not be able to tell that switchboard
	// forgot something.
	full := n.text + timelineSep + text
	if errors.Is(err, notify.ErrSendFullText) {
		rerr := n.client.Replace(ctx, n.ref, full, idem+idemFull)
		if rerr == nil {
			n.text = full
			return observability.MonitorNotifyReplaced, nil
		}
		if !errors.Is(rerr, notify.ErrEditUnsupported) && !errors.Is(rerr, notify.ErrNoSuchMessage) {
			return "", rerr
		}
	}
	// Nothing addressable is left — this platform cannot edit, or the
	// message is gone. Start again, carrying the whole story so the
	// thread that survives is the readable one.
	n.ref, n.text = notify.Ref{}, ""
	return n.post(ctx, full, idem+idemNew)
}

// post opens a new timeline. Caller holds mu.
func (n *notifier) post(ctx context.Context, text, idem string) (string, error) {
	ref, err := n.client.Post(ctx, n.conv, text, idem)
	if err != nil {
		return "", err
	}
	n.ref, n.text = ref, text
	return observability.MonitorNotifyPosted, nil
}

// cycleFailed tells the channel that the monitoring itself is broken —
// once, on the edge from working to failing.
//
// This is the gap W4.2 left open and named: a collection that fails
// every fifteen minutes is invisible to everyone who is not reading the
// daemon's logs or its error counter, and "nobody is reading anything"
// is the operating assumption of the whole feature. It is also the one
// message in this file that no model wrote, so it says plainly that it
// is mast talking.
//
// Edge-triggered rather than rate-limited: a broken monitor that
// re-announces itself on a cadence trains an operator to mute the
// channel, which costs them the incident report too.
func (n *notifier) cycleFailed(ctx context.Context, tick time.Time, cause error) {
	if !n.enabled() || cause == nil {
		return
	}
	n.mu.Lock()
	if n.failing {
		n.mu.Unlock()
		return
	}
	n.failing = true
	// A failure notice is its own message, and it ends whatever story
	// was being told: the next assessment starts fresh rather than
	// appending after an apology.
	n.ref, n.text = notify.Ref{}, ""
	n.mu.Unlock()
	n.health(ctx, tick, fmt.Sprintf(
		"mast could not complete the %s monitoring cycle for %s: %v\n\nNothing was assessed for that tick. The cadence continues, and this notice is sent once rather than on every failed cycle — watch mast_scheduled_fires_total{outcome=\"error\"} for the rate.",
		n.workload, tick.UTC().Format(time.RFC3339), cause))
}

// cycleRecovered says the monitoring is working again, once, on the way
// back. Sent even when the recovered cycle is quiet: an operator who
// was told the monitor was broken is owed the message that it is not,
// and a quiet cycle would otherwise say nothing at all.
func (n *notifier) cycleRecovered(ctx context.Context, tick time.Time) {
	if !n.enabled() {
		return
	}
	n.mu.Lock()
	if !n.failing {
		n.mu.Unlock()
		return
	}
	n.failing = false
	n.mu.Unlock()
	n.health(ctx, tick, fmt.Sprintf(
		"mast completed the %s monitoring cycle for %s. Monitoring has recovered.",
		n.workload, tick.UTC().Format(time.RFC3339)))
}

// health posts one mast-authored notice. It never returns an error: the
// caller is already reporting a failure (or has just stopped), and a
// notice that could not be delivered must not become the thing that
// fails the fire.
func (n *notifier) health(ctx context.Context, tick time.Time, text string) {
	if _, err := n.client.Post(ctx, n.conv, text, n.idem(tick)+idemHealth); err != nil {
		n.obs.MonitorNotify(n.workload, observability.MonitorNotifyError)
		n.logger.Error("could not tell the chat ingress about the monitoring cycle's health",
			"conversation", n.conv, "error", err.Error())
		return
	}
	n.obs.MonitorNotify(n.workload, observability.MonitorNotifyHealth)
	n.logger.Info("told the chat ingress about the monitoring cycle's health",
		"conversation", n.conv, "tick", tick.UTC().Format(time.RFC3339))
}

// idem is this tick's replay key. The tick rather than the wall clock,
// so the key a retry presents is the key the original send used.
func (n *notifier) idem(tick time.Time) string {
	return fmt.Sprintf("mast:%s:%s", n.workload, tick.UTC().Format(time.RFC3339))
}

// digestWake counts a cycle the deadman woke. Separate from the send so
// it is counted whether or not the send then succeeded.
func (n *notifier) digestWake() {
	if !n.enabled() {
		return
	}
	n.obs.MonitorDigestWake(n.workload)
}
