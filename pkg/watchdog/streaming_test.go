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

package watchdog

import (
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// What a streaming turn actually puts on the wire, read out of ADK
// v2.2.0 rather than imagined. `internal/llminternal/stream_aggregator.go`:
//
//   - every provider chunk is yielded with Partial = true (line 94),
//     carrying the raw parts the provider sent;
//   - the one non-partial response is the aggregate built at Close
//     (line ~305), whose parts come from the accumulated sequence;
//   - `base_flow.go:630-636` yields the partial event to the consumer
//     and only *then* runs `if resp.Partial { continue }` ahead of
//     handleFunctionCalls.
//
// So a consumer of the event stream sees each function call twice: once
// on a partial, once on the aggregate ADK runs it from.
//
// Two shapes, because the aggregator has two paths and they fail
// differently (`processFunctionCallPart`, line 129):

// partialStreamingArgsChunk is the PartialArgs path — the provider
// streams the argument object field by field, so the chunk names the
// function and carries NO Args. Nothing assembles them until Close.
func partialStreamingArgsChunk(name, id string) *session.Event {
	ev := eventWithCalls(&genai.Part{FunctionCall: &genai.FunctionCall{
		ID:          id,
		Name:        name,
		PartialArgs: []*genai.PartialArg{{JsonPath: "$.pattern", StringValue: "foo"}},
	}})
	ev.Partial = true
	return ev
}

// partialWholeCallChunk is the other path — the provider sends the call
// whole, and the aggregator appends the *same part pointer* it just
// yielded. Args and ID therefore match the aggregate exactly.
func partialWholeCallChunk(call *genai.FunctionCall) *session.Event {
	ev := eventWithCalls(&genai.Part{FunctionCall: call})
	ev.Partial = true
	return ev
}

// aggregate is the Close response: not partial, arguments assembled.
func aggregate(call *genai.FunctionCall) *session.Event {
	return eventWithCalls(&genai.Part{FunctionCall: call})
}

// The headline: one call, counted once, on the event ADK runs it from.
//
// This is the ID-less shape — Gemini does not populate FunctionCall.ID
// — so the dedup set falls back to name+args, the partial's args are
// empty and the aggregate's are not, and the two get different keys.
// Pre-fix the watchdog sees two calls where the model made one, and a
// repeat detector counting across turns treats that as a loop.
func TestObserveEvent_CountsAStreamedCallOnce(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	final := &genai.FunctionCall{Name: "grep", Args: map[string]any{"pattern": "foo"}}

	seen := map[string]struct{}{}
	ObserveEvent(w, partialStreamingArgsChunk("grep", ""), seen)
	ObserveEvent(w, aggregate(final), seen)

	if got := len(w.observed); got != 1 {
		t.Fatalf("a streamed call was observed %d times, want 1: %+v", got, w.observed)
	}
	if got := w.observed[0].Args; got != `{"pattern":"foo"}` {
		t.Errorf("observed args = %s, want the assembled arguments", got)
	}
}

// The case the dedup set cannot reach, and the reason this is not
// merely a double count. When the provider DOES send a call ID, the
// partial and the aggregate share it, so `seen` collapses them — and
// collapses them onto the FIRST one, which is the chunk whose arguments
// have not been assembled yet. The count is right and the content is
// wrong: every call to this tool records `{}`, so the literal-compare
// detector reads two genuinely different calls as a repeat.
func TestObserveEvent_AStreamedCallKeepsItsArguments(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	final := &genai.FunctionCall{ID: "fc-1", Name: "grep", Args: map[string]any{"pattern": "foo"}}

	seen := map[string]struct{}{}
	ObserveEvent(w, partialStreamingArgsChunk("grep", "fc-1"), seen)
	ObserveEvent(w, aggregate(final), seen)

	if got := len(w.observed); got != 1 {
		t.Fatalf("observed %d times, want 1: %+v", got, w.observed)
	}
	if got := w.observed[0].Args; got == "{}" {
		t.Fatalf("the partial chunk's empty arguments won the dedup; " +
			"two different calls to one tool would compare equal")
	} else if got != `{"pattern":"foo"}` {
		t.Errorf("observed args = %s, want the assembled arguments", got)
	}
}

// The whole-call path was already safe, by dedup rather than by the
// guard — worth pinning so nobody concludes the guard is what fixed it
// and worth stating because the issue named this path as the defect.
// It passes both pre-fix and post-fix, deliberately.
func TestObserveEvent_TheWholeCallPathWasAlreadyDeduped(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	call := &genai.FunctionCall{Name: "grep", Args: map[string]any{"pattern": "foo"}}

	seen := map[string]struct{}{}
	ObserveEvent(w, partialWholeCallChunk(call), seen)
	ObserveEvent(w, aggregate(call), seen)

	if got := len(w.observed); got != 1 {
		t.Fatalf("observed %d times, want 1: %+v", got, w.observed)
	}
}

// A partial that carries a function response should not be counted
// either. ADK reaches handleFunctionCalls only past the partial guard,
// so this should never arrive — the check costs a field read and the
// alternative is a failure streak firing at half its threshold.
func TestObserveToolResults_SkipsPartials(t *testing.T) {
	t.Parallel()
	w := &failCountingWatchdog{}
	resp := &genai.FunctionResponse{
		ID:       "fc-1",
		Name:     "grep",
		Response: map[string]any{"error": "boom"},
	}

	partial := eventWithCalls(&genai.Part{FunctionResponse: resp})
	partial.Partial = true
	seen := map[string]struct{}{}
	if ObserveToolResults(w, partial, seen) {
		t.Error("a partial event reported an observation")
	}
	ObserveToolResults(w, eventWithCalls(&genai.Part{FunctionResponse: resp}), seen)

	if got := len(w.results); got != 1 {
		t.Fatalf("observed %d results, want 1: %+v", got, w.results)
	}
}

// The guard must not swallow the event: a partial still reaches the
// consumer, it is only not counted. Tap wraps a stream that other
// subsystems read, and a watchdog that ate the model's output would be
// a far louder bug than the one being fixed.
func TestTap_PartialsStillReachTheConsumer(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	final := &genai.FunctionCall{Name: "grep", Args: map[string]any{"pattern": "foo"}}
	evPartial := partialStreamingArgsChunk("grep", "")
	evFinal := aggregate(final)

	var got []*session.Event
	for ev, err := range Tap(seq(pair(evPartial, nil), pair(evFinal, nil)), w, nil) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("consumer saw %d events, want both", len(got))
	}
	if got[0] != evPartial || got[1] != evFinal {
		t.Error("the events the consumer saw are not the ones that went in")
	}
	if n := len(w.observed); n != 1 {
		t.Errorf("watchdog observed %d calls across the pair, want 1", n)
	}
}

// failCountingWatchdog records results so ObserveToolResults has a
// ToolResultObserver to reach; the fake in bridge_test.go implements
// only the base interface.
type failCountingWatchdog struct {
	fakeWatchdog
	results []ToolResult
}

func (f *failCountingWatchdog) ObserveToolResult(r ToolResult) {
	f.results = append(f.results, r)
}
