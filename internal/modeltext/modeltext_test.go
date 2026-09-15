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

package modeltext

import (
	"testing"

	"google.golang.org/genai"
)

// This is the predicate's own table. It proves the predicate works and nothing
// else — that the nine call sites actually use it is pinned one test per site,
// next to each site, because eight of the nine were an omission rather than a
// mistake and a shared helper cannot fail for a caller who never calls it.
func TestText(t *testing.T) {
	for _, tc := range []struct {
		name string
		part *genai.Part
		want string
		ok   bool
	}{
		{"nil part", nil, "", false},
		{"plain text", &genai.Part{Text: "the pod is crashlooping"}, "the pod is crashlooping", true},
		{"empty text", &genai.Part{}, "", false},
		{"thinking block", &genai.Part{Text: "let me check the registry", Thought: true}, "", false},
		{
			// Anthropic's redacted thinking: the payload is entirely in the
			// signature and there is no text at all. It must read as "not
			// caller-facing" and not as "an empty answer".
			"redacted thinking", &genai.Part{Thought: true, ThoughtSignature: []byte("opaque")}, "", false,
		},
		{
			// A signature on a part that is NOT flagged Thought is ordinary
			// answer text a provider happened to sign. Filtering on the
			// signature instead of the flag would drop the answer.
			"signed answer", &genai.Part{Text: "restarted it", ThoughtSignature: []byte("sig")}, "restarted it", true,
		},
		{"function call", &genai.Part{FunctionCall: &genai.FunctionCall{Name: "scale"}}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Text(tc.part)
			if got != tc.want || ok != tc.ok {
				t.Errorf("Text() = (%q, %t), want (%q, %t)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
