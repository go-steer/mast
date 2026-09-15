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

// Package modeltext reads the caller-facing text out of a model response
// part (#370).
//
// It exists because "the model's answer" and "the text parts of the model's
// content" are not the same set, and the difference is a thinking block. A
// provider that reasons returns its reasoning as a text part flagged
// Thought, and that part is there for the provider's own replay — Anthropic
// requires the assistant turn preceding a tool_result to carry its thinking
// blocks back with their signatures intact — not for a caller to read.
//
// Before this package the distinction was made in exactly one place
// (pkg/agent's stall detector) and omitted in eight others, including the
// A2A result artifact handed to another team's agent, the AG-UI
// TextMessage stream, and the eval trace whose FinalText the graders read.
// Nothing leaked, because claude-opus-5 under the request mast sends today
// returns a thinking block with a signature and an EMPTY body. That is a
// property of the vendor's default and not of this repo: the same model
// asked with `{"type":"adaptive","display":"summarized"}` returns the
// reasoning text populated. So the eight omissions were one vendor default
// away from being a disclosure, and a predicate everyone calls is worth
// more than eight comments nobody has to read.
//
// Reasoning is not permanently unreachable — docs/ag-ui-design.md OQ 5
// plans an opt-in per bundle. This is the floor that opt-in opts in FROM,
// and a surface that decides to publish reasoning should read Thought
// parts deliberately rather than by not filtering them.
package modeltext

import "google.golang.org/genai"

// Text reports the caller-facing text carried by one part of a model's
// content, and whether there is any.
//
// ok is false for three things: a nil part, a part with no text at all
// (a function call or response), and a thinking block — including a
// redacted one, whose payload rides in ThoughtSignature with no text to
// return. Callers should branch on ok rather than on the emptiness of the
// returned string, so that "the model said nothing" and "the model thought
// something I must not repeat" stay distinguishable at the call site.
func Text(p *genai.Part) (string, bool) {
	if p == nil || p.Thought || p.Text == "" {
		return "", false
	}
	return p.Text, true
}
