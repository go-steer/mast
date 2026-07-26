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

package budget

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// usageEvent fakes one model call's worth of streamed usage — the
// shape the meter sees from the runner event stream.
func usageEvent(totalTokens int32) *session.Event {
	return &session.Event{
		LLMResponse: model.LLMResponse{
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				TotalTokenCount: totalTokens,
			},
		},
	}
}

func TestMaxTurnsTrips(t *testing.T) {
	m := NewMeter(Limits{MaxTurns: 2})
	if err := m.Observe(usageEvent(10)); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if err := m.Observe(usageEvent(10)); err != nil {
		t.Fatalf("turn 2 (at cap, not over): %v", err)
	}
	err := m.Observe(usageEvent(10))
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("turn 3: want ErrExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "turns") {
		t.Errorf("error should name the turn cap: %v", err)
	}
	if _, _, calls := m.Snapshot(); calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// Events without UsageMetadata (function responses, control events)
// are free: they are neither billed nor counted as turns.
func TestNonModelEventsAreNotTurns(t *testing.T) {
	m := NewMeter(Limits{MaxTurns: 1})
	for i := 0; i < 5; i++ {
		if err := m.Observe(&session.Event{}); err != nil {
			t.Fatalf("free event %d: %v", i, err)
		}
	}
	if err := m.Observe(nil); err != nil {
		t.Fatalf("nil event: %v", err)
	}
	if err := m.Observe(usageEvent(10)); err != nil {
		t.Fatalf("first real turn should be under a cap of 1: %v", err)
	}
}

func TestZeroMaxTurnsMeansUnlimited(t *testing.T) {
	m := NewMeter(Limits{})
	for i := 0; i < 100; i++ {
		if err := m.Observe(usageEvent(10)); err != nil {
			t.Fatalf("turn %d with no limits: %v", i, err)
		}
	}
}

func TestCostCapTrips(t *testing.T) {
	// 1000 tokens/call at $0.05/1K = $0.05/call; a $0.12 cap survives
	// two calls and trips on the third.
	m := NewMeter(Limits{MaxCostUSD: 0.12, RatePer1K: 0.05})
	if err := m.Observe(usageEvent(1000)); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if err := m.Observe(usageEvent(1000)); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if err := m.Observe(usageEvent(1000)); !errors.Is(err, ErrExceeded) {
		t.Fatalf("call 3: want ErrExceeded, got %v", err)
	}
}

func TestTokenCapTrips(t *testing.T) {
	m := NewMeter(Limits{MaxTokens: 150})
	if err := m.Observe(usageEvent(100)); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if err := m.Observe(usageEvent(100)); !errors.Is(err, ErrExceeded) {
		t.Fatalf("call 2: want ErrExceeded, got %v", err)
	}
}
