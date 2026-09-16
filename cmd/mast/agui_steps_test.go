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

// Tests for the AG-UI step bracket (#98): STEP_STARTED / STEP_FINISHED naming
// the agent that authored each stretch of a run. See (*aguiEmitter).openStep
// for why a step is authorship here rather than the design text's "turn-N".

import (
	"context"
	"slices"
	"testing"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/agui"
)

// mkAuthoredEvent builds a model event attributed to an agent — the shape the
// runner produces for every dispatch shape mast builds (pkg/specialists, and
// the seam pkg/budget buckets spend by).
func mkAuthoredEvent(author, text string) *adksession.Event {
	ev := mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}})
	ev.Author = author
	return ev
}

// stepTrace renders the step frames in a captured stream as "+name" for a
// start and "-name" for a finish, so an assertion reads as the bracket
// sequence a client would see.
func stepTrace(frames []any) []string {
	var out []string
	for _, f := range frames {
		switch s := f.(type) {
		case agui.StepStarted:
			out = append(out, "+"+s.StepName)
		case agui.StepFinished:
			out = append(out, "-"+s.StepName)
		}
	}
	return out
}

// TestAGUIEmitterStepsFollowAuthorship is the core pin: the bracket opens on a
// change of author, closes the previous one first, stays open across successive
// events from the same author, and reopens for an author seen before. The last
// open bracket is closed by closeStep, not by onEvent, because only the caller
// knows the turn is over.
//
// Neutralize checks: drop the openStep call in onEvent and no step frame
// appears; drop the closeStep inside openStep and the finishes vanish; drop the
// `author == e.step` guard and the repeated author opens a second bracket.
func TestAGUIEmitterStepsFollowAuthorship(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit}

	e.onEvent(mkAuthoredEvent("coordinator", "dispatching"))
	e.onEvent(mkAuthoredEvent("coordinator", "still me"))
	e.onEvent(mkAuthoredEvent("log-analyzer", "found it"))
	e.onEvent(mkAuthoredEvent("coordinator", "summarizing"))
	e.closeStep()

	want := []string{
		"+coordinator",
		"-coordinator", "+log-analyzer",
		"-log-analyzer", "+coordinator",
		"-coordinator",
	}
	if trace := stepTrace(*got); !slices.Equal(trace, want) {
		t.Fatalf("step trace = %v, want %v", trace, want)
	}
}

// TestAGUIEmitterStepOpensBeforeTheEventItBrackets pins the ordering that makes
// the bracket mean anything: the answer an agent produced must arrive inside
// that agent's step, not after it closed. Neutralize check: move the openStep
// call below the text triad and the STEP_STARTED lands after the message.
func TestAGUIEmitterStepOpensBeforeTheEventItBrackets(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit}

	e.onEvent(mkAuthoredEvent("triage", "hello"))
	e.closeStep()

	want := []string{"StepStarted", "TextMessageStart", "TextMessageContent", "TextMessageEnd", "StepFinished"}
	if seq := aguiFrameTypes(*got); !slices.Equal(seq, want) {
		t.Fatalf("frame order = %v, want %v", seq, want)
	}
}

// TestAGUIEmitterStepIgnoresNonAgentEvents pins the two cases that must NOT
// name a step. An unauthored event cannot name one at all. A non-model event
// carrying an author — a tool response echoed back onto the stream — belongs
// inside the step of the agent that called the tool, so it must not close that
// step and reopen a new one under the echo's author.
//
// This is why openStep reads Author only on a model-authored event: it makes
// "the author is an agent" true by construction, with no roster set to thread
// into the emitter and keep in step.
func TestAGUIEmitterStepIgnoresNonAgentEvents(t *testing.T) {
	t.Run("unauthored model event", func(t *testing.T) {
		emit, got := collectEmit()
		e := &aguiEmitter{emit: emit}
		e.onEvent(mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "hi"}}}))
		e.closeStep()
		if trace := stepTrace(*got); len(trace) != 0 {
			t.Fatalf("step trace = %v, want none (the event names no author)", trace)
		}
	})

	t.Run("authored tool echo stays inside the caller's step", func(t *testing.T) {
		emit, got := collectEmit()
		e := &aguiEmitter{emit: emit}
		e.onEvent(mkAuthoredEvent("triage", "calling"))
		fr := genai.NewPartFromFunctionResponse("search", map[string]any{"hits": 3})
		fr.FunctionResponse.ID = "call-1"
		echo := mkEvent(&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{fr}})
		echo.Author = "search"
		e.onEvent(echo)
		e.closeStep()

		want := []string{"+triage", "-triage"}
		if trace := stepTrace(*got); !slices.Equal(trace, want) {
			t.Fatalf("step trace = %v, want %v (the tool echo must not open a step)", trace, want)
		}
	})
}

// TestAGUIEmitterCloseStepWithNoOpenStep pins that closeStep on a run that
// produced no model event emits nothing — a refused or empty turn must not
// ship an orphaned STEP_FINISHED — and that a second call is a no-op.
// Neutralize check: drop the `e.step == ""` guard and both cases emit a frame
// naming the empty string.
func TestAGUIEmitterCloseStepWithNoOpenStep(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit}

	e.closeStep()
	if len(*got) != 0 {
		t.Fatalf("closeStep with no open step emitted %v, want nothing", aguiFrameTypes(*got))
	}

	e.onEvent(mkAuthoredEvent("triage", "hi"))
	e.closeStep()
	e.closeStep()
	if trace := stepTrace(*got); !slices.Equal(trace, []string{"+triage", "-triage"}) {
		t.Fatalf("step trace = %v, want one balanced pair (the second closeStep must be a no-op)", trace)
	}
}

// TestAGUIRunBracketsTheAuthoringAgent drives a real turn through runTurnPre
// and requires the step to be named after the agent the runner actually ran.
//
// The unit tests above hand-build events, so they prove the bracketing logic
// and nothing about the premise underneath it — that session.Event.Author is
// populated with the agent's name on the AG-UI path at all. That premise is
// load-bearing (the whole design rests on it) and it lives in ADK, not here, so
// it gets a test that reads it off a real runner rather than a fixture. The
// harness's root agent is named pause_abort_agent, and that string has to come
// back out of the stream.
//
// Neutralize check: drop RunAgent's closeStep and the run ends with an open
// bracket; make openStep read ev.Branch instead of ev.Author and the name is
// empty, so no step is emitted at all.
func TestAGUIRunBracketsTheAuthoringAgent(t *testing.T) {
	b := newAGUIBackendRunner(t, &blockableModel{})
	emit, got := collectEmit()

	if _, err := b.RunAgent(context.Background(), agui.RunInput{
		ThreadID: "t1", RunID: "r1", Text: "investigate",
	}, emit); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	frames := *got
	want := []string{"+pause_abort_agent", "-pause_abort_agent"}
	if trace := stepTrace(frames); !slices.Equal(trace, want) {
		t.Fatalf("step trace = %v, want %v", trace, want)
	}

	// The bracket must enclose the answer, and must sit inside the opening
	// frames the backend emits before the turn.
	seq := aguiFrameTypes(frames)
	start := slices.Index(seq, "StepStarted")
	finish := slices.Index(seq, "StepFinished")
	text := slices.Index(seq, "TextMessageContent")
	if start < 2 {
		t.Fatalf("StepStarted at %d in %v, want after RunStarted+StateSnapshot", start, seq)
	}
	if start >= text || text >= finish {
		t.Fatalf("answer at %d is not inside the bracket [%d,%d] in %v", text, start, finish, seq)
	}
	if finish != len(seq)-1 {
		t.Fatalf("StepFinished at %d of %v, want last (the terminal frame is the server's)", finish, seq)
	}
}
