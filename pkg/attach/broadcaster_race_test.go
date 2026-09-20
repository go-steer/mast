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

// Originally derived from go-steer/core-agent@25d8531cf8d1d69459471009a9e7e2e9b0dff1e2

package attach

import (
	"context"
	"iter"
	"sync"
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/eventlog"
)

// floodStream is a test eventlog.Stream whose Since and Watch both
// yield a dense, unbounded-until-n run of entries starting just past
// fromSeq. It lets the broadcaster's two delivery sources — the
// per-subscriber replayThenTail (Since) and the shared pump (Watch) —
// race over the same subscriber channel/map without needing a real DB.
type floodStream struct {
	n int // entries to yield from each of Since/Watch
}

func (floodStream) Append(context.Context, session.Session, *session.Event) (int64, error) {
	return 0, nil
}

func (s floodStream) Since(ctx context.Context, fromSeq int64, _ ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return func(yield func(eventlog.Entry, error) bool) {
		for i := int64(1); i <= int64(s.n); i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if !yield(eventlog.Entry{Seq: fromSeq + i, Event: &session.Event{}}, nil) {
				return
			}
		}
	}
}

func (s floodStream) Watch(ctx context.Context, fromSeq int64, _ ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return func(yield func(eventlog.Entry, error) bool) {
		for i := int64(1); i <= int64(s.n); i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if !yield(eventlog.Entry{Seq: fromSeq + i, Event: &session.Event{}}, nil) {
				return
			}
		}
		<-ctx.Done()
	}
}

func (floodStream) Close() error { return nil }

// TestBroadcaster_DualSourceSend_NoRace pins the fix for #377: the
// per-subscriber replayThenTail goroutine used to call send() WITHOUT
// holding b.mu, while the shared pump goroutine iterated b.subs and
// sent under b.mu. A slow SSE consumer (full buffer) during replay made
// both goroutines call detachLocked() — delete(b.subs) + close(sub.ch)
// — concurrently with the pump's locked send/iterate, producing "send
// on closed channel", "concurrent map writes", or "concurrent map
// iteration and map write" and crashing the whole daemon.
//
// The subscriber channel is deliberately tiny (cap 1) and never
// drained, so the buffer fills immediately and detach fires from both
// the replay and pump paths at once. Run under `go test -race`: before
// the fix this reliably trips the race detector (or panics); after it,
// both sources funnel through b.mu and it stays clean.
func TestBroadcaster_DualSourceSend_NoRace(t *testing.T) {
	t.Parallel()

	const iterations = 300

	for run := 0; run < iterations; run++ {
		b := &broadcaster{
			entry:  &Entry{AppName: "core-agent", UserID: "u", SessionID: "test"},
			stream: floodStream{n: 64},
			query:  nil,
		}
		// Tiny, undrained buffer → the very next send after the first
		// fills it, forcing detachLocked from whichever source wins.
		sub := &subscriber{
			ch:       make(chan Frame, 1),
			since:    0,
			lastSent: 0,
		}
		b.subs = map[*subscriber]struct{}{sub: {}}

		ctx, cancel := context.WithCancel(context.Background())

		// Emulate Subscribe's lazy-pump wiring so pump's terminal
		// "no subscribers left" branch can clear b.cancel cleanly.
		b.cancel = cancel

		var wg sync.WaitGroup
		wg.Add(2)
		// Shared pump: locks b.mu, iterates b.subs, sends.
		go func() {
			defer wg.Done()
			b.pump(ctx, b.pumpGen, 0)
		}()
		// Per-subscriber replay+tail: the goroutine that used to send
		// unlocked.
		go func() {
			defer wg.Done()
			b.replayThenTail(ctx, sub, 0)
		}()

		wg.Wait()
		cancel() // no-op if pump already cleared it; releases any tail wait
	}
}

// TestBroadcaster_SubscribeCapabilitiesAlwaysFirst pins the #385 SSE
// boot-frame ordering fix: Subscribe must enqueue the spec-required
// capabilities frame into the new subscriber's channel BEFORE the
// subscriber becomes visible to any live producer. Pre-fix, Subscribe
// registered the subscriber in b.subs (and started the pump) and only
// THEN delivered boot frames — a concurrent Emit (or pump broadcast)
// in that window put a typed live frame ahead of capabilities. The
// test floods typed events from another goroutine while subscribing
// repeatedly and asserts the first frame is always capabilities.
// Run under -race.
func TestBroadcaster_SubscribeCapabilitiesAlwaysFirst(t *testing.T) {
	t.Parallel()

	const iterations = 200

	for run := 0; run < iterations; run++ {
		b := &broadcaster{
			entry:   &Entry{AppName: "core-agent", UserID: "u", SessionID: "boot-order"},
			stream:  floodStream{n: 16},
			subs:    make(map[*subscriber]struct{}),
			closing: make(chan struct{}),
		}

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b.Emit(EventStatusUpdate, StatusUpdate{TurnState: TurnStateStreaming})
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		ch := b.Subscribe(ctx, 0)
		first, ok := <-ch
		if !ok {
			t.Fatalf("run %d: channel closed before any frame", run)
		}
		if first.Type != EventCapabilities {
			t.Fatalf("run %d: first frame = type=%q seq=%d, want the %q boot frame first",
				run, first.Type, first.Seq, EventCapabilities)
		}

		close(stop)
		wg.Wait()
		cancel()
		b.Close()
		// Drain to release the (now closed) channel cleanly.
		for range ch { //nolint:revive // draining
		}
	}
}

// TestBroadcaster_BootFramesRaceWithPump pins the companion half of
// #377: deliverBootFrames sent the capabilities/status/usage frames via
// sendTyped WITHOUT b.mu, even though the subscriber was already in
// b.subs and the pump could be broadcasting to it concurrently. A full
// buffer during boot then raced detachLocked against the pump. This
// drives deliverBootFrames concurrently with the pump against a tiny
// channel; clean under -race only with the fix.
func TestBroadcaster_BootFramesRaceWithPump(t *testing.T) {
	t.Parallel()

	const iterations = 300

	for run := 0; run < iterations; run++ {
		b := &broadcaster{
			entry:  &Entry{AppName: "core-agent", UserID: "u", SessionID: "test"},
			stream: floodStream{n: 64},
		}
		sub := &subscriber{
			ch:       make(chan Frame, 1),
			since:    0,
			lastSent: 0,
		}
		b.subs = map[*subscriber]struct{}{sub: {}}

		ctx, cancel := context.WithCancel(context.Background())
		b.cancel = cancel

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.pump(ctx, b.pumpGen, 0)
		}()
		go func() {
			defer wg.Done()
			b.deliverBootFrames(ctx, sub)
		}()

		wg.Wait()
		cancel()
	}
}

// TestBroadcaster_PumpStartCursorNotShared pins #448: the pump's start
// cursor used to be a broadcaster FIELD (b.startedAt), written by
// register under b.mu and read by the pump goroutine with no lock at
// all — once for the debug line, once as the argument to Stream.Watch.
//
// The interleaving is the reconnect. detachLocked cancels the pump's
// context and nils b.cancel when the last subscriber leaves, which
// makes the NEXT register a first-subscriber registration again: it
// writes the cursor and spawns a successor. The outgoing pump
// goroutine need not have reached its own read yet — a cancelled
// context does not unschedule a goroutine, and the read happens before
// Watch is ever entered. An SSE client that disconnects and
// reconnects is enough.
//
// mast's pumpGen stamp does not cover this. A generation counter
// orders the dying pump's teardown sweep against its successor's
// state; it says nothing about an unsynchronized field read, and the
// gen here is deliberately the pre-register one so the sweep is
// correctly inert.
//
// Pre-fix this trips the race detector (WRITE at register / READ at
// pump, no happens-before between them) within a handful of the
// iterations below. Post-fix the cursor is a pump parameter handed
// over at spawn, so there is no shared location left to race on.
// Requires -race; without it the test asserts nothing.
func TestBroadcaster_PumpStartCursorNotShared(t *testing.T) {
	t.Parallel()

	const iterations = 300

	for run := 0; run < iterations; run++ {
		b := &broadcaster{
			entry:  &Entry{AppName: "core-agent", UserID: "u", SessionID: "test"},
			stream: floodStream{}, // yields nothing, then waits on ctx
			subs:   map[*subscriber]struct{}{},
		}

		// The outgoing pump. Its context is already cancelled and
		// b.cancel is already nil because detachLocked did both when
		// the last subscriber left — which is precisely why the
		// register below takes the first-subscriber branch.
		staleCtx, staleCancel := context.WithCancel(context.Background())
		staleCancel()
		staleGen := b.pumpGen

		// Both goroutines are released from one barrier so neither
		// side's start happens-before the other's.
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			b.pump(staleCtx, staleGen, 0)
		}()
		sub := &subscriber{ch: make(chan Frame, 1)}
		go func() {
			defer wg.Done()
			<-start
			if registered, _ := b.register(sub, 7); registered {
				// register reserves a replayThenTail slot on b.wg for
				// its caller; this test is the caller and does not run
				// one, so release it or Close would wait forever.
				b.wg.Done()
			}
		}()
		close(start)
		wg.Wait()

		b.Close() // cancels and drains the successor pump
	}
}
