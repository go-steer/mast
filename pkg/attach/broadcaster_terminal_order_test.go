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

package attach

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/eventlog"
)

// slowStream is an eventlog.Stream double that models the race #327 is
// about: the log knows about an event (LatestSeq reports it) before the
// pump has handed it to anyone. head and the frames channel are driven
// independently, which is exactly the window a real pump's 200ms poll
// leaves open and which no amount of sleeping against real SQLite
// schedules reliably.
type slowStream struct {
	mu        sync.Mutex
	head      int64
	headErr   error
	headCalls int

	frames chan eventlog.Entry
}

func newSlowStream() *slowStream {
	return &slowStream{frames: make(chan eventlog.Entry, 8)}
}

func (s *slowStream) setHead(seq int64) {
	s.mu.Lock()
	s.head = seq
	s.mu.Unlock()
}

func (s *slowStream) LatestSeq(context.Context, ...eventlog.QueryOption) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headCalls++
	return s.head, s.headErr
}

func (s *slowStream) HeadCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headCalls
}

func (s *slowStream) Append(context.Context, session.Session, *session.Event) (int64, error) {
	return 0, errors.New("slowStream: Append not supported")
}

func (s *slowStream) Since(context.Context, int64, ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return func(func(eventlog.Entry, error) bool) {}
}

func (s *slowStream) Watch(ctx context.Context, _ int64, _ ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return func(yield func(eventlog.Entry, error) bool) {
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-s.frames:
				if !yield(e, nil) {
					return
				}
			}
		}
	}
}

func (s *slowStream) Close() error { return nil }

var _ eventlog.Stream = (*slowStream)(nil)

// blindStream is a slowStream with no LatestSeq — the "no seq-index
// extension" degrade path. Embedding would promote the method, so the
// field is named and the wrapper deliberately does not forward it.
type blindStream struct{ inner *slowStream }

func (s *blindStream) Append(ctx context.Context, sess session.Session, ev *session.Event) (int64, error) {
	return s.inner.Append(ctx, sess, ev)
}

func (s *blindStream) Since(ctx context.Context, from int64, opts ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return s.inner.Since(ctx, from, opts...)
}

func (s *blindStream) Watch(ctx context.Context, from int64, opts ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	return s.inner.Watch(ctx, from, opts...)
}

func (s *blindStream) Close() error { return s.inner.Close() }

var _ eventlog.Stream = (*blindStream)(nil)

func terminalOrderBroadcaster(t *testing.T, stream eventlog.Stream) *broadcaster {
	t.Helper()
	b, err := newBroadcaster(&Entry{
		AppName:   "mast",
		UserID:    "u",
		SessionID: "s-order",
		Agent: &eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "mast", user: "u", sid: "s-order"},
			handle:         &eventlog.Handle{Stream: stream},
		},
	})
	if err != nil {
		t.Fatalf("newBroadcaster: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

// collect drains ch for d, returning a label per frame: the typed
// event's name, or "seq:<n>" for a legacy eventlog frame.
func collect(ch <-chan Frame, d time.Duration) []string {
	var got []string
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return got
			}
			if f.Type != "" {
				got = append(got, f.Type)
				continue
			}
			got = append(got, "seq:"+itoa(f.Seq))
		case <-deadline:
			return got
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// The #327 regression. The adapter publishes turn-complete the instant
// RunTurn returns; the turn's final assistant text goes to the
// eventlog and reaches subscribers on the pump's next poll. Nothing
// held the terminal frame for the log, so "this turn is over" could
// arrive ahead of the answer it terminates — and a consumer that
// finalizes its render on turn-complete drops the text.
//
// Verified to FAIL on pre-fix code: without the barrier the collected
// order is [capabilities status-update turn-complete seq:7].
func TestTerminalFrameWaitsForTheAnswer(t *testing.T) {
	t.Parallel()
	stream := newSlowStream()
	b := terminalOrderBroadcaster(t, stream)
	ch := b.Subscribe(context.Background(), 0)

	// The turn's last event is in the log but has not been pumped.
	stream.setHead(7)
	go func() {
		time.Sleep(40 * time.Millisecond)
		stream.frames <- eventlog.Entry{Seq: 7, Event: session.NewEvent(context.Background(), "answer")}
	}()

	b.Emit(EventTurnComplete, TurnComplete{PromptID: "p-1"})

	got := collect(ch, 500*time.Millisecond)
	want := []string{EventCapabilities, EventStatusUpdate, "seq:7", EventTurnComplete}
	if !sameStrings(got, want) {
		t.Errorf("frame order = %v, want %v — turn-complete overtook the answer it terminates", got, want)
	}
}

// turn-error takes the same barrier: a turn that failed has its own
// final events in the log, and a consumer that renders the error and
// stops reading loses them the same way.
func TestTerminalErrorFrameWaitsForTheAnswer(t *testing.T) {
	t.Parallel()
	stream := newSlowStream()
	b := terminalOrderBroadcaster(t, stream)
	ch := b.Subscribe(context.Background(), 0)

	stream.setHead(3)
	go func() {
		time.Sleep(40 * time.Millisecond)
		stream.frames <- eventlog.Entry{Seq: 3, Event: session.NewEvent(context.Background(), "partial")}
	}()

	b.Emit(EventTurnError, TurnError{Message: "boom"})

	got := collect(ch, 500*time.Millisecond)
	want := []string{EventCapabilities, EventStatusUpdate, "seq:3", EventTurnError}
	if !sameStrings(got, want) {
		t.Errorf("frame order = %v, want %v", got, want)
	}
}

// Only the terminal frames wait. A status-update or usage-update
// carries no ordering claim about the log, and holding every typed
// event for a poll interval would make the whole stream laggy to buy
// nothing.
func TestNonTerminalFramesDoNotWait(t *testing.T) {
	t.Parallel()
	stream := newSlowStream()
	b := terminalOrderBroadcaster(t, stream)
	ch := b.Subscribe(context.Background(), 0)
	stream.setHead(99) // nothing will ever deliver this

	before := stream.HeadCalls()
	start := time.Now()
	b.Emit(EventStatusUpdate, StatusUpdate{TurnState: TurnStateStreaming})
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("Emit(status-update) took %s, want no barrier at all", d)
	}
	if after := stream.HeadCalls(); after != before {
		t.Errorf("Emit(status-update) queried the log head %d times, want 0", after-before)
	}
	got := collect(ch, 100*time.Millisecond)
	if len(got) == 0 || got[len(got)-1] != EventStatusUpdate {
		t.Errorf("frames = %v, want the status-update delivered", got)
	}
}

// Every failure path degrades to the pre-fix unordered delivery rather
// than hanging or dropping the frame. A late frame is a correctness
// bug; a frame that never arrives, or a daemon whose turn loop is
// wedged behind a broken eventlog, is worse.
func TestTerminalFrameDegradesRatherThanHangs(t *testing.T) {
	// Not parallel: the deadline case rewrites the package-level seam.
	t.Run("no seq-index extension", func(t *testing.T) {
		stream := newSlowStream()
		b := terminalOrderBroadcaster(t, &blindStream{inner: stream})
		ch := b.Subscribe(context.Background(), 0)
		stream.setHead(50)
		start := time.Now()
		b.Emit(EventTurnComplete, TurnComplete{})
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Errorf("Emit took %s against a stream with no LatestSeq, want an immediate degrade", d)
		}
		assertSawTerminal(t, ch)
	})

	t.Run("head query error", func(t *testing.T) {
		stream := newSlowStream()
		stream.headErr = errors.New("database is locked")
		b := terminalOrderBroadcaster(t, stream)
		ch := b.Subscribe(context.Background(), 0)
		stream.setHead(50)
		start := time.Now()
		b.Emit(EventTurnComplete, TurnComplete{})
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Errorf("Emit took %s with a failing head query, want an immediate degrade", d)
		}
		assertSawTerminal(t, ch)
	})

	t.Run("empty log", func(t *testing.T) {
		stream := newSlowStream() // head stays 0
		b := terminalOrderBroadcaster(t, stream)
		ch := b.Subscribe(context.Background(), 0)
		start := time.Now()
		b.Emit(EventTurnComplete, TurnComplete{})
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Errorf("Emit took %s against an empty log, want an immediate degrade", d)
		}
		assertSawTerminal(t, ch)
	})

	t.Run("no subscribers", func(t *testing.T) {
		stream := newSlowStream()
		stream.setHead(50)
		b := terminalOrderBroadcaster(t, stream)
		start := time.Now()
		b.Emit(EventTurnComplete, TurnComplete{})
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Errorf("Emit took %s with nobody attached, want no barrier", d)
		}
		if calls := stream.HeadCalls(); calls != 0 {
			t.Errorf("head queried %d times with no subscribers, want 0", calls)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		restore := terminalFrameWait
		terminalFrameWait = 60 * time.Millisecond
		defer func() { terminalFrameWait = restore }()

		stream := newSlowStream()
		b := terminalOrderBroadcaster(t, stream)
		ch := b.Subscribe(context.Background(), 0)
		stream.setHead(50) // never delivered
		start := time.Now()
		b.Emit(EventTurnComplete, TurnComplete{})
		d := time.Since(start)
		if d < 40*time.Millisecond {
			t.Errorf("Emit took %s, want it to have actually waited", d)
		}
		if d > 2*time.Second {
			t.Errorf("Emit took %s, want the bound to release it", d)
		}
		assertSawTerminal(t, ch)
	})

	t.Run("shutdown", func(t *testing.T) {
		restore := terminalFrameWait
		terminalFrameWait = 10 * time.Second
		defer func() { terminalFrameWait = restore }()

		stream := newSlowStream()
		b := terminalOrderBroadcaster(t, stream)
		b.Subscribe(context.Background(), 0)
		stream.setHead(50) // never delivered
		go func() {
			time.Sleep(30 * time.Millisecond)
			b.Close()
		}()
		start := time.Now()
		b.Emit(EventTurnComplete, TurnComplete{})
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("Emit took %s across a shutdown, want b.closing to release the barrier", d)
		}
	})
}

func assertSawTerminal(t *testing.T, ch <-chan Frame) {
	t.Helper()
	got := collect(ch, 200*time.Millisecond)
	for _, f := range got {
		if f == EventTurnComplete || f == EventTurnError {
			return
		}
	}
	t.Errorf("frames = %v, want the terminal frame delivered — degrading must not drop it", got)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
