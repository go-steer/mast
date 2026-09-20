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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/approval"
)

// testParkNotifier builds the egress over the notify fake with a clock a
// test can move, skipping buildParkNotifier so the bucket can be
// exercised without waiting out a real refill.
func testParkNotifier(t *testing.T) (*parkNotifier, *fakeIngress, *time.Time) {
	t.Helper()
	f := &fakeIngress{}
	clock := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	p := &parkNotifier{
		client:     f,
		conv:       "#sre-oncall",
		workload:   "cluster-watch",
		logger:     discardLogger(),
		now:        func() time.Time { return clock },
		tokens:     parkNotifyBurst,
		refilledAt: clock,
	}
	return p, f, &clock
}

func testParkNotice() approval.ParkNotice {
	return approval.ParkNotice{
		Session:    "sess-7f2a",
		Invocation: "e-11c0",
		Workload:   "cluster-watch",
		Agent:      "remediator",
		Tool:       "scale_deployment",
		Key:        "scale_deployment(deployment=api, replicas=10)",
		Hint:       "Approve mutating call scale_deployment(deployment=api, replicas=10)?",
		ParkedAt:   time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC),
	}
}

// TestAParkNoticeSaysEnoughToAnswerIt is the operator-facing half of
// #451. A page that does not name the session is a page that starts with
// the recipient hunting for what they were paged about, which is most of
// the latency the announcement exists to remove.
func TestAParkNoticeSaysEnoughToAnswerIt(t *testing.T) {
	p, f, _ := testParkNotifier(t)
	n := testParkNotice()
	p.announce(context.Background(), n)

	if len(f.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(f.posts))
	}
	// A park is always its own message. Appending would splice it into
	// whatever story the monitoring timeline is telling, and would make a
	// park's visibility depend on the workload having spoken.
	if got := len(f.appends) + len(f.replaces); got != 0 {
		t.Errorf("appends+replaces = %d, want 0: a park must not join the monitoring timeline", got)
	}
	post := f.posts[0]
	if post.Conversation != "#sre-oncall" {
		t.Errorf("conversation = %q, want %q", post.Conversation, "#sre-oncall")
	}
	for _, want := range []string{
		n.Session,
		n.Agent,
		n.Tool,
		n.Hint,
		"mast sessions show " + n.Session,
		"has NOT been made",
	} {
		if !strings.Contains(post.Text, want) {
			t.Errorf("the notice does not contain %q; an operator cannot act on it:\n%s", want, post.Text)
		}
	}
	if post.Idem == "" {
		t.Error("no idempotency key: a send that timed out client-side after landing would double-post")
	}
}

// TestAParkNoticeIsNotFromTheModel keeps the daemon's own voice
// distinguishable from the agent's. Both arrive in the same channel, and
// only one of them is a claim about mast itself.
func TestAParkNoticeIsNotFromTheModel(t *testing.T) {
	p, f, _ := testParkNotifier(t)
	p.announce(context.Background(), testParkNotice())

	text := f.posts[0].Text
	if !strings.HasPrefix(text, "mast stopped to ask for an approval") {
		t.Errorf("the notice does not open by saying who is speaking and why:\n%s", text)
	}
	// The park hint already renders the call key; printing it again as a
	// structured field puts the (elided) arguments in the message twice.
	if n := strings.Count(text, "scale_deployment(deployment=api, replicas=10)"); n != 1 {
		t.Errorf("the call key appears %d times, want 1:\n%s", n, text)
	}
}

// TestTheSameParkIsSentUnderTheSameKey, and two different parks are not.
// The replay key is what stops a send that timed out after landing from
// paging an operator twice for one decision.
func TestTheSameParkIsSentUnderTheSameKey(t *testing.T) {
	n := testParkNotice()
	// Same park, built twice: the key must not depend on anything that
	// changes between the two, or the retry it exists for double-posts.
	if parkNotifyIdem(n) != parkNotifyIdem(testParkNotice()) {
		t.Fatal("the replay key is not stable for one park")
	}

	other := n
	other.Key = "scale_deployment(deployment=api, replicas=11)"
	if parkNotifyIdem(n) == parkNotifyIdem(other) {
		t.Error("two different calls parked in one turn share a replay key; the second would be swallowed as a duplicate")
	}

	later := n
	later.Invocation = "e-22d1"
	if parkNotifyIdem(n) == parkNotifyIdem(later) {
		t.Error("the same call parked again in a later turn reuses the earlier replay key; the second park would never be announced")
	}
}

// TestAWorkloadCanOnlyExhaustItsOwnAnnouncements. A model that has found
// a way to park in a loop is the case this bucket exists for: the cost
// of not having one is an operator muting the channel, which costs them
// the park that mattered too.
func TestAWorkloadCanOnlyExhaustItsOwnAnnouncements(t *testing.T) {
	p, f, clock := testParkNotifier(t)
	for i := 0; i < parkNotifyBurst+4; i++ {
		p.announce(context.Background(), testParkNotice())
	}
	if len(f.posts) != parkNotifyBurst {
		t.Fatalf("posts = %d, want %d: the burst is not a ceiling", len(f.posts), parkNotifyBurst)
	}

	// Throttling is a pause, not a permanent gag — a workload that parks
	// hard for a minute must still be heard an hour later.
	*clock = clock.Add(parkNotifyRefill)
	p.announce(context.Background(), testParkNotice())
	if len(f.posts) != parkNotifyBurst+1 {
		t.Fatalf("posts = %d after one refill period, want %d: the bucket never refills, so one loop silences parks for the life of the process", len(f.posts), parkNotifyBurst+1)
	}

	// And the refill is a rate, not a reset.
	p.announce(context.Background(), testParkNotice())
	if len(f.posts) != parkNotifyBurst+1 {
		t.Errorf("posts = %d, want %d: one elapsed period granted more than one token", len(f.posts), parkNotifyBurst+1)
	}

	// A long idle refills to the burst and no further.
	*clock = clock.Add(100 * parkNotifyRefill)
	for i := 0; i < parkNotifyBurst+2; i++ {
		p.announce(context.Background(), testParkNotice())
	}
	if want := 2*parkNotifyBurst + 1; len(f.posts) != want {
		t.Errorf("posts = %d, want %d: an idle daemon banked more than a full burst", len(f.posts), want)
	}
}

// TestAFailedAnnouncementIsSwallowed. The gate has already parked and the
// park is already answerable; the only thing a returned error could do
// here is turn a chat outage into a turn failure.
func TestAFailedAnnouncementIsSwallowed(t *testing.T) {
	p, f, _ := testParkNotifier(t)
	f.postErr = errors.New("switchboard 503")
	p.announce(context.Background(), testParkNotice())

	if len(f.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(f.posts))
	}
	// Nor does a failure latch. A chat ingress that was down for one park
	// must not have taken the announcement path down with it.
	p.announce(context.Background(), testParkNotice())
	if len(f.posts) != 2 {
		t.Fatalf("posts = %d after the ingress recovered, want 2", len(f.posts))
	}

	// The failed send did spend a token — it reached the ingress and mast
	// cannot know whether it landed — so the burst is now two down, not
	// one.
	for i := 0; i < parkNotifyBurst; i++ {
		p.announce(context.Background(), testParkNotice())
	}
	if len(f.posts) != parkNotifyBurst {
		t.Errorf("posts = %d, want %d", len(f.posts), parkNotifyBurst)
	}
}

// TestNoEgressIsSilentNotFatal covers the ordinary daemon: no
// --park-notify, so no notifier, and the gate's hook calls into a nil
// receiver on every park.
func TestNoEgressIsSilentNotFatal(t *testing.T) {
	p, err := buildParkNotifier(discardLogger(), nil, "cluster-watch", "", nil)
	if err != nil {
		t.Fatalf("buildParkNotifier with no conversation: %v", err)
	}
	if p != nil {
		t.Fatalf("notifier = %v, want nil", p)
	}
	p.announce(context.Background(), testParkNotice()) // must not panic
}

// TestAConversationWithNoIngressRefusesToStart. An operator who
// configured --park-notify has told us they are not watching a console,
// so warning on the console and carrying on hands them exactly the
// silence they configured against.
func TestAConversationWithNoIngressRefusesToStart(t *testing.T) {
	_, err := buildParkNotifier(discardLogger(), nil, "cluster-watch", "#sre-oncall", nil)
	if err == nil {
		t.Fatal("buildParkNotifier accepted a conversation with no chat ingress; the daemon would run believing parks were announced")
	}
	for _, want := range []string{"#sre-oncall", "--notify-url", notifyTokenEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not say how to fix it: %v", want, err)
		}
	}

	// The startup check and the constructor must agree, or serve refuses
	// on one path and not the other.
	if got := parkNotifyConfigError("#sre-oncall", nil); got == nil {
		t.Error("parkNotifyConfigError disagrees with buildParkNotifier: serve would bind a listener and only then fail")
	}
	if got := parkNotifyConfigError("  ", nil); got != nil {
		t.Errorf("a blank conversation is not configuration: %v", got)
	}
	if got := parkNotifyConfigError("#sre-oncall", &fakeIngress{}); got != nil {
		t.Errorf("a configured conversation with an ingress is valid: %v", got)
	}
}
