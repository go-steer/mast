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
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/a2a"
	"github.com/go-steer/mast/pkg/agui"
)

// These are the cmd/mast half of #370: four places that read model text off a
// runner event and forwarded a thinking block with it. They are grouped in one
// file rather than filed next to each subject because the defect is one
// omission repeated, not four unrelated bugs — and because the ninth site
// somebody adds should have an obvious place to be tested.
//
// Part order is what decides whether a given site leaks, so each case is seeded
// with the order that discriminates it. Three of the four take the provider's
// usual shape — the thinking block FIRST, then whatever the caller is allowed
// to see — because they accumulate every text part (AG-UI, both A2A channels)
// or keep the first (logEvent). runOneShot keeps the LAST text part instead, so
// under that shape it is correct by accident; it is given an answer followed by
// a thinking block. mast does not choose the order: ADK hands these loops one
// aggregated content per model response, in whatever order the provider
// streamed it.

// thoughtCanary is the reasoning no caller may see. A literal rather than a
// production constant on purpose: the assertion has to fail if the filter
// stops working, and it must not be possible to make it pass by editing the
// code under test.
const thoughtCanary = "<reasoning> the operator's key is hunter2, and I should not say so"

// thinkingPart is a provider's thinking block, signed the way Anthropic and
// Gemini sign one for replay.
func thinkingPart(text string) *genai.Part {
	return &genai.Part{Text: text, Thought: true, ThoughtSignature: []byte("sig-370")}
}

// TestAGUIEmitterSkipsThinking pins that a thinking block never reaches an
// AG-UI client, neither as a TextMessage frame nor through RunFinished.result
// (which the server reads off lastText).
//
// agui.emit_reasoning (#98) does not weaken this and is not an exception to
// it. That opt-in publishes reasoning as its own REASONING_* frames; a thought
// still never becomes answer text and still never becomes the run's result,
// whether the opt-in is on or off. This emitter has it off.
//
// Neutralize check: restore `part.Text != ""` in aguiEmitter.onEvent and the
// content frame reads "<reasoning>…the answer", failing both assertions.
func TestAGUIEmitterSkipsThinking(t *testing.T) {
	emit, got := collectEmit()
	e := &aguiEmitter{emit: emit}
	e.onEvent(mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		thinkingPart(thoughtCanary),
		{Text: "the deploy is crashlooping on an image pull"},
	}}))

	var contents []string
	for _, f := range *got {
		if c, ok := f.(agui.TextMessageContent); ok {
			contents = append(contents, c.Delta)
		}
	}
	if len(contents) != 1 {
		t.Fatalf("text content frames = %d (%q), want 1", len(contents), contents)
	}
	if strings.Contains(contents[0], thoughtCanary) {
		t.Errorf("AG-UI content frame carries the thinking block: %q", contents[0])
	}
	if contents[0] != "the deploy is crashlooping on an image pull" {
		t.Errorf("AG-UI content frame = %q, want the visible text alone", contents[0])
	}
	if strings.Contains(e.lastText, thoughtCanary) {
		t.Errorf("RunFinished.result would carry the thinking block: %q", e.lastText)
	}
}

// TestTurnCaptureSkipsThinking pins the A2A result artifact. turnCapture.lastText
// is what runTurnPre returns and the server publishes as the turn's deliverable,
// so a leak here is durable: it lands in the task's artifact, not just on a
// transient stream frame.
//
// Neutralize check: restore `part.Text != ""` in turnCapture.onEvent and
// lastText is the reasoning concatenated with the answer.
func TestTurnCaptureSkipsThinking(t *testing.T) {
	c := &turnCapture{}
	c.onEvent(mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		thinkingPart(thoughtCanary),
		{Text: "restarted the deployment"},
	}}))
	if strings.Contains(c.lastText, thoughtCanary) {
		t.Errorf("A2A result artifact carries the thinking block: %q", c.lastText)
	}
	if c.lastText != "restarted the deployment" {
		t.Errorf("lastText = %q, want the visible text alone", c.lastText)
	}

	// A turn whose only text is reasoning has no answer to publish. Keeping
	// the previous lastText would be wrong too, so assert the empty case
	// directly rather than inferring it from the absence of the canary.
	c2 := &turnCapture{}
	c2.onEvent(mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		thinkingPart(thoughtCanary),
	}}))
	if c2.lastText != "" {
		t.Errorf("thinking-only turn captured lastText %q, want empty", c2.lastText)
	}
}

// TestEmitStreamProgressSkipsThinking pins the A2A streaming channel: progress
// narration is the model's text, never its reasoning, and a thinking-only event
// produces no frame at all rather than an empty one.
//
// Neutralize check: restore `part.Text != ""` in emitStreamProgress and the
// first frame carries the reasoning and the second frame appears.
func TestEmitStreamProgressSkipsThinking(t *testing.T) {
	var got []*a2a.TaskStatusUpdateEvent
	emit := func(ev any) {
		su, ok := ev.(*a2a.TaskStatusUpdateEvent)
		if !ok {
			t.Fatalf("emit got %T, want *a2a.TaskStatusUpdateEvent", ev)
		}
		got = append(got, su)
	}
	emitStreamProgress(emit, "a2a-x", "c1", 0, mkEvent(&genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{
			thinkingPart(thoughtCanary),
			{Text: "checking the image tag"},
		},
	}))
	emitStreamProgress(emit, "a2a-x", "c1", 1, mkEvent(&genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{thinkingPart(thoughtCanary)},
	}))

	if len(got) != 1 {
		t.Fatalf("progress frames = %d, want 1 (the thinking-only event emits nothing)", len(got))
	}
	if text := got[0].Status.Message.Parts[0].Text; text != "checking the image tag" {
		t.Errorf("progress text = %q, want the visible text alone", text)
	}
}

// TestLogEventSkipsThinking pins the operator log. This one is the widest of
// the four: logEvent runs on every runner event on every server path, and its
// sink is wherever the operator ships logs — so a leak here writes the model's
// reasoning into a retained store that nobody thinks of as a transcript.
//
// logEvent takes the FIRST readable part, so the provider's own ordering is
// what makes it leak: the thinking block is part zero.
//
// Neutralize check: restore `part.Text != ""` and summary is the reasoning.
func TestLogEventSkipsThinking(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ev := mkEvent(&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		thinkingPart(thoughtCanary),
		{Text: "image tag not found"},
	}})
	logEvent(logger, ev, "sess-1")

	out := buf.String()
	if strings.Contains(out, thoughtCanary) {
		t.Errorf("operator log carries the thinking block:\n%s", out)
	}
	if !strings.Contains(out, "image tag not found") {
		t.Errorf("operator log lost the visible text:\n%s", out)
	}

	// A thinking-only event is summarised as having no text, which is the
	// honest answer: there is nothing the operator may be shown.
	buf.Reset()
	logEvent(logger, mkEvent(&genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{thinkingPart(thoughtCanary)},
	}), "sess-1")
	if !strings.Contains(buf.String(), "(no text)") {
		t.Errorf("thinking-only event summary = %s, want (no text)", buf.String())
	}
}

// TestRunOneShotSkipsThinking pins what `mast run` prints, end to end through
// a real runner and a real model.LLM — the scripted provider replaying a turn
// whose parts are an answer followed by a thinking block.
//
// The ordering is the point. This site keeps the LAST readable text part rather
// than accumulating, so under thinking-then-answer it is correct by accident:
// the answer overwrites the reasoning whether or not the filter is there. mast
// does not control the order. ADK hands this loop one aggregated content per
// model response and the parts arrive in whatever order the provider streamed
// them, so a site that is only correct under one vendor's convention is not
// correct — and the cost here is the highest-visibility one in #370, the chain
// of thought printed on stdout as the agent's answer.
//
// The other shape a reader will reach for — a turn whose ONLY text is the
// thinking block — is not testable here and is worth recording: ADK does not
// treat such a response as a finished turn. It re-asks, on both the Chat and
// the Task class root, until the script runs out.
//
// Neutralize check: restore `part.Text != ""` in runOneShot's event loop and
// stdout is the reasoning instead of the answer.
func TestRunOneShotSkipsThinking(t *testing.T) {
	script := filepath.Join(t.TempDir(), "thinking.jsonl")
	line := `{"responses":[{"content":{"role":"model","parts":[` +
		`{"text":"the deploy job is crashlooping on an image pull"},` +
		`{"text":` + quoteJSON(thoughtCanary) + `,"thought":true,"thoughtSignature":"c2lnLTM3MA=="}` +
		`]},"turnComplete":true,"finishReason":"STOP"}]}` + "\n"
	if err := os.WriteFile(script, []byte(line), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("MAST_SCRIPT", script)

	var out bytes.Buffer
	if err := runOneShot(context.Background(), discardLogger(), oneShotOptions{
		Class:      "chat",
		Model:      "scripted",
		SessionDrv: "sqlite",
		Prompt:     "why does the deploy job crashloop?",
	}, &out); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	if strings.Contains(out.String(), thoughtCanary) {
		t.Errorf("one-shot printed the thinking block as the answer: %q", out.String())
	}
	if strings.TrimSpace(out.String()) != "the deploy job is crashlooping on an image pull" {
		t.Errorf("one-shot output = %q, want the visible answer alone", out.String())
	}
}

// quoteJSON renders s as a JSON string literal for the hand-written script
// line above. strconv.Quote is Go syntax, not JSON syntax; they agree for this
// input, but writing the script by hand and then quoting it with the wrong
// grammar is the kind of thing that makes a fixture silently stop matching.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
