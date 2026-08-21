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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/monitor"
	"github.com/go-steer/mast/pkg/notify"
	"github.com/go-steer/mast/pkg/workload"
)

// fakeIngress stands in for switchboard. It records every request and
// lets a test make the next one of a kind fail, which is how the
// recoveries below (409, 501, rollover) are exercised without an HTTP
// server pretending to be a chat platform.
type fakeIngress struct {
	posts    []fakeSend
	appends  []fakeSend
	replaces []fakeSend

	postErr    error
	appendErr  error
	replaceErr error

	// rollTo, when set, is the continuation ref an append answers with
	// instead of 204.
	rollTo *notify.Ref

	n int
}

type fakeSend struct {
	Conversation string
	ID           string
	Text         string
	Idem         string
}

func (f *fakeIngress) Post(_ context.Context, conversation, text, idem string) (notify.Ref, error) {
	f.posts = append(f.posts, fakeSend{Conversation: conversation, Text: text, Idem: idem})
	if err := f.postErr; err != nil {
		f.postErr = nil
		return notify.Ref{}, err
	}
	f.n++
	return notify.Ref{Conversation: conversation, ID: fmt.Sprintf("m%d", f.n)}, nil
}

func (f *fakeIngress) Replace(_ context.Context, ref notify.Ref, text, idem string) error {
	f.replaces = append(f.replaces, fakeSend{Conversation: ref.Conversation, ID: ref.ID, Text: text, Idem: idem})
	if err := f.replaceErr; err != nil {
		f.replaceErr = nil
		return err
	}
	return nil
}

func (f *fakeIngress) Append(_ context.Context, ref notify.Ref, line, idem string) (notify.Ref, error) {
	f.appends = append(f.appends, fakeSend{Conversation: ref.Conversation, ID: ref.ID, Text: line, Idem: idem})
	if err := f.appendErr; err != nil {
		f.appendErr = nil
		return notify.Ref{}, err
	}
	if f.rollTo != nil {
		rolled := *f.rollTo
		f.rollTo = nil
		return rolled, nil
	}
	return ref, nil
}

func (f *fakeIngress) sends() int { return len(f.posts) + len(f.appends) + len(f.replaces) }

// testNotifier builds a notifier over the fake with a controllable
// clock, skipping newNotifier so a test can set digest_after without
// writing YAML.
func testNotifier(t *testing.T, digest time.Duration) (*notifier, *fakeIngress, *time.Time) {
	t.Helper()
	f := &fakeIngress{}
	clock := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	n := &notifier{
		client:   f,
		conv:     "#sre-oncall",
		digest:   digest,
		workload: "cluster-watch",
		logger:   discardLogger(),
		now:      func() time.Time { return clock },
		spokeAt:  clock,
	}
	return n, f, &clock
}

func changed(n int) *monitor.Set {
	s := &monitor.Set{Scanned: 40}
	for i := 0; i < n; i++ {
		s.Transitions = append(s.Transitions, monitor.Transition{
			SubjectKey: fmt.Sprintf("finding-%d", i), Class: "new",
		})
	}
	return s
}

func quiet() *monitor.Set { return &monitor.Set{Scanned: 40} }

// A cycle whose classifier reported nothing is the case the whole leg
// exists for: no model call, no HTTP request.
func TestNotifyQuietCycleSaysNothing(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	speech, reason := n.decide(quiet())
	if speech != speechQuiet {
		t.Fatalf("speech = %v, want quiet", speech)
	}
	if speech.speaks() {
		t.Error("a quiet cycle reports that it speaks; the model would be woken")
	}
	n.quiet("s1", reason)
	if f.sends() != 0 {
		t.Errorf("a quiet cycle made %d request(s), want none", f.sends())
	}
}

func TestNotifySpeaksWhenSomethingChanged(t *testing.T) {
	n, _, _ := testNotifier(t, 0)
	speech, reason := n.decide(changed(3))
	if speech != speechSpeak || !speech.speaks() {
		t.Fatalf("speech = %v, want speak", speech)
	}
	if !strings.Contains(reason, "3 transition") {
		t.Errorf("reason = %q, want it to count the transitions", reason)
	}
}

// A workload that names no transitions source has told mast nothing
// about what "unchanged" means, so mast has no basis to be quiet — and
// inventing one (diffing the collected results itself) is the domain
// knowledge pkg/monitor refuses to hold.
func TestNotifyWithoutAClassificationSpeaksEveryCycle(t *testing.T) {
	n, _, _ := testNotifier(t, time.Hour)
	if speech, _ := n.decide(nil); speech != speechSpeak {
		t.Errorf("speech = %v, want speak for a workload that classifies nothing", speech)
	}
}

// The deadman: silence is bounded by wall-clock, so a monitor that has
// been quiet for a week is distinguishable from one that died.
func TestNotifyDigestWakesAfterTheDeadline(t *testing.T) {
	n, _, clock := testNotifier(t, 6*time.Hour)
	if speech, _ := n.decide(quiet()); speech != speechQuiet {
		t.Fatalf("speech = %v, want quiet before the deadline", speech)
	}
	*clock = clock.Add(6 * time.Hour)
	speech, reason := n.decide(quiet())
	if speech != speechDigest || !speech.speaks() {
		t.Fatalf("speech = %v, want digest once the deadline passed", speech)
	}
	if !strings.Contains(reason, "nothing changed") {
		t.Errorf("reason = %q, want it to say the cycle had no news", reason)
	}
	// And a digest that spoke resets the clock, so the deadman is "how
	// long since anything was said", not "how long since a change".
	if err := n.speak(context.Background(), "s1", "all quiet", "k1"); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if speech, _ := n.decide(quiet()); speech != speechQuiet {
		t.Errorf("speech = %v, want quiet again right after a digest", speech)
	}
}

// digest_after is opt-in: a workload that did not set one is willing to
// stay silent for as long as nothing changes.
func TestNotifyNoDigestMeansNoDeadman(t *testing.T) {
	n, _, clock := testNotifier(t, 0)
	*clock = clock.Add(30 * 24 * time.Hour)
	if speech, _ := n.decide(quiet()); speech != speechQuiet {
		t.Errorf("speech = %v after a month of quiet, want quiet (no deadman configured)", speech)
	}
}

// The timeline: consecutive speaking cycles extend one message, and the
// first quiet cycle ends it so the next incident starts its own.
func TestNotifyTimelineGrowsThenClosesOnQuiet(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	ctx := context.Background()
	if err := n.speak(ctx, "s1", "two findings are new", "k1"); err != nil {
		t.Fatalf("first speak: %v", err)
	}
	if len(f.posts) != 1 || f.posts[0].Conversation != "#sre-oncall" {
		t.Fatalf("posts = %+v, want one post to the configured conversation", f.posts)
	}
	if err := n.speak(ctx, "s2", "one of them escalated", "k2"); err != nil {
		t.Fatalf("second speak: %v", err)
	}
	if len(f.posts) != 1 || len(f.appends) != 1 {
		t.Fatalf("posts=%d appends=%d, want the second cycle to extend the first message", len(f.posts), len(f.appends))
	}
	if f.appends[0].ID != "m1" {
		t.Errorf("appended to %q, want the posted message m1", f.appends[0].ID)
	}
	n.quiet("s3", "nothing changed")
	if err := n.speak(ctx, "s4", "a new one appeared", "k4"); err != nil {
		t.Fatalf("speak after quiet: %v", err)
	}
	if len(f.posts) != 2 {
		t.Errorf("posts = %d, want a quiet cycle to close the timeline so the next incident posts fresh", len(f.posts))
	}
}

// 409 "no remembered text": the ingress forgot the body, so the whole
// timeline is re-sent as an edit — including what earlier cycles said,
// which is the part a client that only re-sent the new line would lose.
func TestNotifyAppendFallsBackToTheFullText(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	ctx := context.Background()
	if err := n.speak(ctx, "s1", "first", "k1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	f.appendErr = &notify.Error{Status: 409, Sentinel: notify.ErrSendFullText}
	if err := n.speak(ctx, "s2", "second", "k2"); err != nil {
		t.Fatalf("speak after a 409: %v", err)
	}
	if len(f.replaces) != 1 {
		t.Fatalf("replaces = %+v, want one full-text edit", f.replaces)
	}
	if got := f.replaces[0].Text; got != "first"+timelineSep+"second" {
		t.Errorf("replaced with %q, want the whole timeline", got)
	}
	if f.replaces[0].Idem == f.appends[0].Idem {
		t.Error("the fallback reused the append's replay key; switchboard fingerprints the body against it and would answer 409")
	}
	// And the timeline keeps growing from the re-sent body.
	if err := n.speak(ctx, "s3", "third", "k3"); err != nil {
		t.Fatalf("speak after the fallback: %v", err)
	}
	if len(f.appends) != 2 || f.appends[1].Text != "third" {
		t.Errorf("appends = %+v, want the timeline still open", f.appends)
	}
}

// 501 on an append means the same thing permanently, and the recovery
// is the same edit.
func TestNotifyAppendUnsupportedFallsBackToTheFullText(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	ctx := context.Background()
	if err := n.speak(ctx, "s1", "first", "k1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	f.appendErr = &notify.Error{Status: 501, Sentinel: notify.ErrSendFullText}
	if err := n.speak(ctx, "s2", "second", "k2"); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(f.replaces) != 1 {
		t.Errorf("replaces = %+v, want the 501-on-append to become an edit", f.replaces)
	}
}

// Rollover: switchboard posted a continuation because the message was
// full. Every later append has to target the continuation, or it 409s
// forever.
func TestNotifyRolloverRetargetsTheTimeline(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	ctx := context.Background()
	if err := n.speak(ctx, "s1", "first", "k1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	f.rollTo = &notify.Ref{Conversation: "#sre-oncall", ID: "m1-cont"}
	if err := n.speak(ctx, "s2", "second", "k2"); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if err := n.speak(ctx, "s3", "third", "k3"); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(f.appends) != 2 || f.appends[1].ID != "m1-cont" {
		t.Fatalf("appends = %+v, want the third cycle to target the continuation", f.appends)
	}
	// The continuation's body starts from the line that rolled into it,
	// so a later full-text fallback re-sends the continuation, not the
	// whole history a second time.
	f.appendErr = &notify.Error{Status: 409, Sentinel: notify.ErrSendFullText}
	if err := n.speak(ctx, "s4", "fourth", "k4"); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if got := f.replaces[0].Text; got != "second"+timelineSep+"third"+timelineSep+"fourth" {
		t.Errorf("replaced with %q, want only what the continuation holds", got)
	}
}

// A platform that cannot edit at all leaves nothing addressable, so the
// story restarts as a new message carrying what came before.
func TestNotifyEditUnsupportedPostsAFreshTimeline(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	ctx := context.Background()
	if err := n.speak(ctx, "s1", "first", "k1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	f.appendErr = &notify.Error{Status: 409, Sentinel: notify.ErrSendFullText}
	f.replaceErr = &notify.Error{Status: 501, Sentinel: notify.ErrEditUnsupported}
	if err := n.speak(ctx, "s2", "second", "k2"); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(f.posts) != 2 {
		t.Fatalf("posts = %+v, want a second message", f.posts)
	}
	if got := f.posts[1].Text; got != "first"+timelineSep+"second" {
		t.Errorf("posted %q, want the whole story carried into the new message", got)
	}
	if f.posts[1].Idem == f.posts[0].Idem {
		t.Error("the fresh post reused the first post's replay key")
	}
}

// A message somebody deleted is not recoverable by editing it.
func TestNotifyDeletedMessagePostsAgain(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	ctx := context.Background()
	if err := n.speak(ctx, "s1", "first", "k1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	f.appendErr = &notify.Error{Status: 404, Sentinel: notify.ErrNoSuchMessage}
	if err := n.speak(ctx, "s2", "second", "k2"); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(f.replaces) != 0 {
		t.Errorf("replaces = %+v, want no edit against a message that is gone", f.replaces)
	}
	if len(f.posts) != 2 {
		t.Errorf("posts = %d, want a replacement message", len(f.posts))
	}
}

// A refused send is reported, and the timeline is closed: mast no
// longer knows what the message in the channel says, so the next cycle
// must not append to it.
func TestNotifyFailureClosesTheTimeline(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	ctx := context.Background()
	if err := n.speak(ctx, "s1", "first", "k1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	f.appendErr = &notify.Error{Status: 403, Sentinel: notify.ErrDenied}
	if err := n.speak(ctx, "s2", "second", "k2"); err == nil {
		t.Fatal("speak reported success after the ingress refused it")
	}
	if err := n.speak(ctx, "s3", "third", "k3"); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(f.posts) != 2 || f.posts[1].Text != "third" {
		t.Errorf("posts = %+v, want the next cycle to start a new message with only its own news", f.posts)
	}
}

// A turn that halted or parked produced no sentence. mast does not
// write one on the model's behalf — a report with no content looks like
// a report.
func TestNotifyRefusesToInventAnAssessment(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	err := n.speak(context.Background(), "s1", "   \n ", "k1")
	if err == nil {
		t.Fatal("speak accepted an empty assessment")
	}
	if !strings.Contains(err.Error(), "produced no text") {
		t.Errorf("error = %v, want it to name the empty turn", err)
	}
	if f.sends() != 0 {
		t.Errorf("sent %d request(s) for an empty assessment", f.sends())
	}
}

// The health notice: a monitor that is broken says so once, not every
// fifteen minutes, and says so again when it recovers.
func TestNotifyHealthNoticesAreEdgeTriggered(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	ctx := context.Background()
	tick := time.Date(2026, 8, 21, 3, 15, 0, 0, time.UTC)

	n.cycleRecovered(ctx, tick) // nothing to recover from
	if f.sends() != 0 {
		t.Fatalf("a recovery with no failure sent %d request(s)", f.sends())
	}
	n.cycleFailed(ctx, tick, errors.New("mcp server unreachable"))
	n.cycleFailed(ctx, tick.Add(15*time.Minute), errors.New("mcp server unreachable"))
	n.cycleFailed(ctx, tick.Add(30*time.Minute), errors.New("mcp server unreachable"))
	if len(f.posts) != 1 {
		t.Fatalf("posts = %d, want one notice for a run of failures", len(f.posts))
	}
	if !strings.Contains(f.posts[0].Text, "mcp server unreachable") {
		t.Errorf("notice = %q, want it to carry the cause", f.posts[0].Text)
	}
	n.cycleRecovered(ctx, tick.Add(45*time.Minute))
	n.cycleRecovered(ctx, tick.Add(60*time.Minute))
	if len(f.posts) != 2 {
		t.Fatalf("posts = %d, want exactly one recovery notice", len(f.posts))
	}
	if !strings.Contains(f.posts[1].Text, "recovered") {
		t.Errorf("notice = %q, want it to say the monitoring is back", f.posts[1].Text)
	}
	// And the next failure is announced again: the edge, not a one-shot.
	n.cycleFailed(ctx, tick.Add(75*time.Minute), errors.New("again"))
	if len(f.posts) != 3 {
		t.Errorf("posts = %d, want the second outage announced", len(f.posts))
	}
}

// A failure notice ends whatever story was being told: the next
// assessment starts fresh rather than appending after an apology.
func TestNotifyFailureNoticeClosesTheTimeline(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	ctx := context.Background()
	if err := n.speak(ctx, "s1", "first", "k1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	n.cycleFailed(ctx, time.Now().UTC(), errors.New("boom"))
	if err := n.speak(ctx, "s2", "second", "k2"); err != nil {
		t.Fatalf("speak: %v", err)
	}
	if len(f.appends) != 0 {
		t.Errorf("appends = %+v, want no append across a failure notice", f.appends)
	}
}

// A notice mast cannot deliver must not become the thing that fails the
// fire: the caller is already reporting the real failure.
func TestNotifyHealthNoticeSwallowsItsOwnFailure(t *testing.T) {
	n, f, _ := testNotifier(t, 0)
	f.postErr = errors.New("ingress down too")
	n.cycleFailed(context.Background(), time.Now().UTC(), errors.New("boom"))
	if len(f.posts) != 1 {
		t.Fatalf("posts = %d, want the attempt", len(f.posts))
	}
	// The edge still flipped, so the recovery notice is still owed.
	n.cycleRecovered(context.Background(), time.Now().UTC())
	if len(f.posts) != 2 {
		t.Errorf("posts = %d, want the recovery notice after a failed failure notice", len(f.posts))
	}
}

// The whole file is nil-safe: a workload with no notify block holds a
// nil *notifier through every call site rather than guarding each one.
func TestNotifyNilIsTheUnconfiguredWorkload(t *testing.T) {
	var n *notifier
	if n.enabled() {
		t.Error("a nil notifier reports enabled")
	}
	speech, _ := n.decide(quiet())
	if speech != speechUnconfigured {
		t.Errorf("speech = %v, want unconfigured", speech)
	}
	if speech.speaks() {
		t.Error("an unconfigured cycle reports that it speaks")
	}
	// None of these may panic, and speak must not fail a fire.
	n.quiet("s1", "nothing changed")
	n.digestWake()
	n.cycleFailed(context.Background(), time.Now().UTC(), errors.New("boom"))
	n.cycleRecovered(context.Background(), time.Now().UTC())
	if err := n.speak(context.Background(), "s1", "text", "k1"); err != nil {
		t.Errorf("speak on an unconfigured workload = %v, want nil", err)
	}
}

func TestNewNotifier(t *testing.T) {
	f := &fakeIngress{}
	mon := workload.Monitor{Notify: &workload.MonitorNotify{Conversation: "#ops", DigestAfter: "6h"}}
	n, err := newNotifier(discardLogger(), nil, "wl", mon, f)
	if err != nil {
		t.Fatalf("newNotifier: %v", err)
	}
	if !n.enabled() || n.conv != "#ops" || n.digest != 6*time.Hour {
		t.Errorf("notifier = %+v, want the bundle's conversation and deadman", n)
	}
	// The deadman counts from construction, so a daemon that boots into
	// a quiet world does not immediately announce the quiet.
	if speech, _ := n.decide(quiet()); speech != speechQuiet {
		t.Errorf("speech = %v on the first cycle after boot, want quiet", speech)
	}
}

func TestNewNotifierRefusals(t *testing.T) {
	f := &fakeIngress{}
	if n, err := newNotifier(discardLogger(), nil, "wl", workload.Monitor{}, f); n != nil || err != nil {
		t.Errorf("newNotifier(no block) = %v, %v; want nil, nil", n, err)
	}
	// A bundle that says to speak against a daemon with nowhere to send
	// it is a monitor an operator believes is reporting.
	mon := workload.Monitor{Notify: &workload.MonitorNotify{Conversation: "#ops"}}
	if _, err := newNotifier(discardLogger(), nil, "wl", mon, nil); err == nil {
		t.Error("newNotifier accepted a notify block with no ingress")
	}
	bad := workload.Monitor{Notify: &workload.MonitorNotify{Conversation: "#ops", DigestAfter: "6 hours"}}
	if _, err := newNotifier(discardLogger(), nil, "wl", bad, f); err == nil {
		t.Error("newNotifier accepted a digest_after that does not parse")
	}
}

func TestBuildNotifyClient(t *testing.T) {
	t.Run("no url is no client", func(t *testing.T) {
		t.Setenv(notifyTokenEnv, "")
		c, err := buildNotifyClient(discardLogger(), "", nil)
		if err != nil {
			t.Fatalf("buildNotifyClient: %v", err)
		}
		// Typed-nil check: a nil *notify.Client boxed into the interface
		// would make every enabled() call true and every cycle panic.
		if c != nil {
			t.Errorf("client = %v, want a nil interface", c)
		}
	})
	t.Run("url without a token", func(t *testing.T) {
		t.Setenv(notifyTokenEnv, "")
		if _, err := buildNotifyClient(discardLogger(), "http://switchboard:8080", nil); err == nil {
			t.Error("buildNotifyClient accepted an ingress URL with no token")
		}
	})
	t.Run("outbound token is an inbound token", func(t *testing.T) {
		t.Setenv(notifyTokenEnv, "shared-secret")
		_, err := buildNotifyClient(discardLogger(), "http://switchboard:8080",
			map[string]string{"MAST_INJECT_TOKEN": "shared-secret"})
		if err == nil {
			t.Fatal("buildNotifyClient accepted a token that also drives this daemon")
		}
		if !strings.Contains(err.Error(), "MAST_INJECT_TOKEN") {
			t.Errorf("error = %v, want it to name the colliding credential", err)
		}
	})
	t.Run("distinct tokens are fine", func(t *testing.T) {
		t.Setenv(notifyTokenEnv, "egress-secret")
		c, err := buildNotifyClient(discardLogger(), "http://switchboard:8080",
			map[string]string{"MAST_INJECT_TOKEN": "inbound-secret", "MAST_A2A_TOKEN": ""})
		if err != nil {
			t.Fatalf("buildNotifyClient: %v", err)
		}
		if c == nil {
			t.Fatal("client = nil, want one")
		}
	})
	t.Run("bad url", func(t *testing.T) {
		t.Setenv(notifyTokenEnv, "egress-secret")
		if _, err := buildNotifyClient(discardLogger(), "switchboard:8080", nil); err == nil {
			t.Error("buildNotifyClient accepted a URL with no scheme")
		}
	})
}
