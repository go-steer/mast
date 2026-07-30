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
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/eventlog"
)

// testHandle opens a throwaway SQLite eventlog so Config validation
// passes; adapter tests never read it.
func testHandle(t *testing.T) *eventlog.Handle {
	t.Helper()
	h, err := eventlog.Open(t.Context(), sqlite.Open(filepath.Join(t.TempDir(), "el.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func baseConfig(t *testing.T, run func(ctx context.Context, msg string) (TurnResult, error)) Config {
	t.Helper()
	return Config{
		AppName:   "mast",
		UserID:    "op",
		SessionID: "s1",
		EventLog:  testHandle(t),
		RunTurn:   run,
	}
}

func TestNewValidation(t *testing.T) {
	run := func(context.Context, string) (TurnResult, error) { return TurnResult{}, nil }
	for name, mutate := range map[string]func(*Config){
		"missing session triple": func(c *Config) { c.SessionID = "" },
		"missing eventlog":       func(c *Config) { c.EventLog = nil },
		"missing runturn":        func(c *Config) { c.RunTurn = nil },
	} {
		cfg := baseConfig(t, run)
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted an invalid Config", name)
		}
	}
	if _, err := New(baseConfig(t, run)); err != nil {
		t.Fatalf("New rejected a valid Config: %v", err)
	}
}

// TestInjectSerializesTurns proves two concurrent Injects never
// overlap RunTurn calls and both run in order.
func TestInjectSerializesTurns(t *testing.T) {
	var mu sync.Mutex
	var order []string
	inFlight := 0
	maxInFlight := 0
	done := make(chan struct{}, 2)

	ad, err := New(baseConfig(t, func(_ context.Context, msg string) (TurnResult, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		order = append(order, msg)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		done <- struct{}{}
		return TurnResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	if err := ad.Inject("first"); err != nil {
		t.Fatal(err)
	}
	if err := ad.Inject("second"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("turns did not complete")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxInFlight != 1 {
		t.Errorf("turns overlapped: max in flight = %d, want 1", maxInFlight)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("turn order = %v, want [first second]", order)
	}
}

// TestInjectAsPropagatesCaller proves the injected caller rides the
// turn context.
func TestInjectAsPropagatesCaller(t *testing.T) {
	got := make(chan auth.Caller, 1)
	ad, err := New(baseConfig(t, func(ctx context.Context, _ string) (TurnResult, error) {
		c, _ := auth.CallerFromContext(ctx)
		got <- c
		return TurnResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.InjectAs("hi", auth.Caller{Identity: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-got:
		if c.Identity != "alice@example.com" {
			t.Errorf("caller identity = %q, want alice@example.com", c.Identity)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn never ran")
	}
}

// TestAttachInterruptCancelsTurn proves POST /interrupt semantics:
// the in-flight turn's ctx is canceled; idle interrupt reports false.
func TestAttachInterruptCancelsTurn(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan error, 1)
	ad, err := New(baseConfig(t, func(ctx context.Context, _ string) (TurnResult, error) {
		close(started)
		<-ctx.Done()
		finished <- ctx.Err()
		return TurnResult{}, ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}

	if ad.AttachInterrupt() {
		t.Error("AttachInterrupt with no turn in flight returned true")
	}
	if err := ad.Inject("long turn"); err != nil {
		t.Fatal(err)
	}
	<-started
	if !ad.AttachInterrupt() {
		t.Error("AttachInterrupt with a turn in flight returned false")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("turn ctx err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt did not cancel the turn")
	}
}

// TestOperatorEventSequence proves the emitter sees the spec order:
// status streaming → turn-complete → status idle, and turn-error on
// failure.
func TestOperatorEventSequence(t *testing.T) {
	turnErr := errors.New("boom")
	fail := false
	done := make(chan struct{}, 1)
	ad, err := New(baseConfig(t, func(context.Context, string) (TurnResult, error) {
		defer func() { done <- struct{}{} }()
		if fail {
			return TurnResult{}, turnErr
		}
		return TurnResult{TokensIn: 3, TokensOut: 7}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var types []string
	var payloads []any
	ad.SetOperatorEventEmitter(func(eventType string, payload any) {
		mu.Lock()
		types = append(types, eventType)
		payloads = append(payloads, payload)
		mu.Unlock()
	})

	waitIdle := func() {
		t.Helper()
		<-done
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			n := len(types)
			last := ""
			if n > 0 {
				last = types[n-1]
			}
			mu.Unlock()
			if last == attach.EventStatusUpdate && n >= 3 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("emitter never saw the terminal status frame; got %v", types)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	if err := ad.Inject("ok turn"); err != nil {
		t.Fatal(err)
	}
	waitIdle()
	mu.Lock()
	if len(types) != 3 || types[0] != attach.EventStatusUpdate || types[1] != attach.EventTurnComplete || types[2] != attach.EventStatusUpdate {
		t.Fatalf("event sequence = %v, want [status-update turn-complete status-update]", types)
	}
	tc, ok := payloads[1].(attach.TurnComplete)
	if !ok || tc.TokensIn != 3 || tc.TokensOut != 7 || tc.PromptID == "" {
		t.Errorf("turn-complete payload = %+v, want tokens 3/7 and a prompt id", payloads[1])
	}
	types, payloads = nil, nil
	mu.Unlock()

	fail = true
	if err := ad.Inject("bad turn"); err != nil {
		t.Fatal(err)
	}
	waitIdle()
	mu.Lock()
	defer mu.Unlock()
	if len(types) != 3 || types[1] != attach.EventTurnError {
		t.Fatalf("event sequence = %v, want turn-error in the middle", types)
	}
}

// TestStatusReflectsTurnState proves AttachStatus flips running/idle.
func TestStatusReflectsTurnState(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	ad, err := New(baseConfig(t, func(context.Context, string) (TurnResult, error) {
		close(started)
		<-release
		return TurnResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := ad.AttachStatus().State; got != "idle" {
		t.Errorf("initial state = %q, want idle", got)
	}
	if err := ad.Inject("x"); err != nil {
		t.Fatal(err)
	}
	<-started
	if got := ad.AttachStatus().State; got != "running" {
		t.Errorf("in-turn state = %q, want running", got)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for ad.AttachStatus().State != "idle" {
		if time.Now().After(deadline) {
			t.Fatal("state never returned to idle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
