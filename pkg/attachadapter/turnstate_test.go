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

package attachadapter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/mast/pkg/attach"
)

// frameLog records the typed events an adapter emits.
type frameLog struct {
	mu     sync.Mutex
	types  []string
	states []string
}

func (f *frameLog) install(ad *Adapter) {
	ad.SetOperatorEventEmitter(func(eventType string, payload any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.types = append(f.types, eventType)
		if su, ok := payload.(attach.StatusUpdate); ok {
			f.states = append(f.states, su.TurnState)
		}
	})
}

// waitForFrames blocks until n frames have arrived.
func (f *frameLog) waitForFrames(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		got := len(f.types)
		types := append([]string(nil), f.types...)
		f.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d frames; got %v", n, types)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *frameLog) turnStates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.states...)
}

// The defect #313 names, at the seam that produced it. The write gate
// parks a mutating call by finishing the turn — RunTurn returns with
// no error and an unanswered interrupt on the transcript — and the
// adapter then published `idle`, the same string a turn that actually
// finished gets. This asserts the TRANSITION, which is what the issue
// asks for: streaming while the turn runs, awaiting_permission the
// moment it parks, and idle again once the park is resolved.
func TestParkedTurnPublishesAwaitingPermission(t *testing.T) {
	var awaiting atomic.Value // stands in for the durable projection
	awaiting.Store("")
	cfg := baseConfig(t, func(context.Context, string) (TurnResult, error) {
		awaiting.Store(attach.TurnStateAwaitingPermission) // the gate parked the call
		return TurnResult{}, nil
	})
	cfg.TurnStateFn = func() string { return awaiting.Load().(string) }
	ad, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	log := &frameLog{}
	log.install(ad)

	if err := ad.Inject("scale the deployment"); err != nil {
		t.Fatal(err)
	}
	log.waitForFrames(t, 3)

	got := log.turnStates()
	want := []string{attach.TurnStateStreaming, attach.TurnStateAwaitingPermission}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("turn states = %v, want %v — a session blocked on a human must not report the string a finished turn reports", got, want)
	}

	// And the state is durable, not a one-shot frame: a client that
	// asks now, or one that connects now and gets a boot snapshot,
	// sees the same thing. This is the switchboard case.
	if s := ad.AttachTurnState(); s != attach.TurnStateAwaitingPermission {
		t.Errorf("AttachTurnState() = %q, want %q", s, attach.TurnStateAwaitingPermission)
	}
	if s := ad.AttachStatus().State; s != attach.AgentStatePaused {
		t.Errorf("AttachStatus().State = %q, want %q", s, attach.AgentStatePaused)
	}

	// The park resolves — by a verdict, an abort, or a stop; the
	// adapter does not care which, it re-reads.
	awaiting.Store("")
	if s := ad.AttachTurnState(); s != attach.TurnStateIdle {
		t.Errorf("after the park resolved: AttachTurnState() = %q, want %q", s, attach.TurnStateIdle)
	}
	if s := ad.AttachStatus().State; s != attach.AgentStateIdle {
		t.Errorf("after the park resolved: AttachStatus().State = %q, want %q", s, attach.AgentStateIdle)
	}
}

// awaiting_elicit is emitted rather than removed, which is the other
// half of #313's "done when": a request_operator_input park can
// produce it, so the constant is a claim mast can now keep.
func TestElicitParkPublishesAwaitingElicit(t *testing.T) {
	cfg := baseConfig(t, func(context.Context, string) (TurnResult, error) { return TurnResult{}, nil })
	cfg.TurnStateFn = func() string { return attach.TurnStateAwaitingElicit }
	ad, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	log := &frameLog{}
	log.install(ad)
	if err := ad.Inject("which cluster?"); err != nil {
		t.Fatal(err)
	}
	log.waitForFrames(t, 3)
	got := log.turnStates()
	if len(got) != 2 || got[1] != attach.TurnStateAwaitingElicit {
		t.Fatalf("turn states = %v, want the terminal frame to be %q", got, attach.TurnStateAwaitingElicit)
	}
}

// A live turn outranks a park. During a resume the interrupt being
// answered is still unresolved on the transcript — its
// FunctionResponse lands inside the turn — so the projection reads
// "awaiting" for the whole run. Letting that win would freeze the
// session on an operator's screen while it works.
func TestARunningTurnOutranksTheDurableProjection(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cfg := baseConfig(t, func(context.Context, string) (TurnResult, error) {
		close(started)
		<-release
		return TurnResult{}, nil
	})
	cfg.TurnStateFn = func() string { return attach.TurnStateAwaitingPermission }
	ad, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.Inject("approve"); err != nil {
		t.Fatal(err)
	}
	<-started
	if s := ad.AttachTurnState(); s != attach.TurnStateStreaming {
		t.Errorf("mid-turn AttachTurnState() = %q, want %q", s, attach.TurnStateStreaming)
	}
	if s := ad.AttachStatus().State; s != attach.AgentStateRunning {
		t.Errorf("mid-turn AttachStatus().State = %q, want %q", s, attach.AgentStateRunning)
	}
	close(release)
}

// Nil TurnStateFn is the pre-#313 shape and must keep working: a
// caller that gave the adapter no way to find out gets the honest
// coarse answer rather than a panic or a fabricated pause.
func TestNoTurnStateHookKeepsTheIdleAnswer(t *testing.T) {
	ad, err := New(baseConfig(t, func(context.Context, string) (TurnResult, error) { return TurnResult{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if s := ad.AttachTurnState(); s != attach.TurnStateIdle {
		t.Errorf("AttachTurnState() = %q, want %q", s, attach.TurnStateIdle)
	}
	if s := ad.AttachStatus().State; s != attach.AgentStateIdle {
		t.Errorf("AttachStatus().State = %q, want %q", s, attach.AgentStateIdle)
	}
}
