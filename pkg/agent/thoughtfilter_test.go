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

package agent

// The pkg/agent half of #370, filed as #372: a ninth place that read model
// text with `p.Text != ""` and would have forwarded a thinking block with it.
//
// It differs from the other eight in one way worth stating, because it is the
// reason the site survived the sweep: it is in TEST code — schemafill_test.go's
// fakeReplyText helper — and nothing a test helper concatenates reaches a user.
// What it can do is decide whether a test passes, and it backs the assertions
// that the offline fakes answer a forced output schema correctly. A helper that
// silently splices reasoning into the string those assertions compare would
// make them agree with the wrong answer.
//
// The shipped fakes emit no thinking block, so this is a latent site and not a
// live leak. #372 decided that stays true: making them emit one was measured on
// #370's branch, broke exactly this helper, and detected nothing that the
// per-site tests do not already detect — so the corpus keeps its thought-shaped
// coverage where each site can pick the part order that discriminates it, and
// the two exported fakes keep the output an embedder may already have pinned.

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// agentThoughtCanary is the reasoning no caller may see. A literal rather than
// a production constant on purpose: the assertion has to fail if the filter
// stops working, and it must not be possible to make it pass by editing the
// code under test.
const agentThoughtCanary = "the schema wants a namespace and I should guess prod"

// thoughtfulFake answers in the shape a reasoning provider actually sends: a
// signed thinking block first, then the answer. Neither shipped fake produces
// one, so the shape has to come from a local double — and fakeReplyText takes
// any model.LLM, so a double is a fair input to it rather than a contrived one.
type thoughtfulFake struct{}

func (thoughtfulFake) Name() string { return "thoughtful" }

func (thoughtfulFake) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{Text: agentThoughtCanary, Thought: true, ThoughtSignature: []byte("sig")},
				{Text: `{"reason":"OOMKilled"}`},
			},
		}}, nil)
	}
}

// TestFakeReplyTextSkipsThinking pins the ninth site (#372, following #370).
//
// The helper accumulates every text part across every response, so it leaks
// under the provider's usual thinking-first shape — the order used here. It is
// the same failure mode as the AG-UI triad and the two A2A channels, which is
// why the seed order matches theirs rather than the last-wins sites'.
//
// Neutralize check: restore `text += p.Text` in fakeReplyText and the returned
// string is the reasoning followed by the answer.
func TestFakeReplyTextSkipsThinking(t *testing.T) {
	got := fakeReplyText(t, thoughtfulFake{}, &model.LLMRequest{})

	if strings.Contains(got, agentThoughtCanary) {
		t.Fatalf("reply = %q, want the answer with no thinking block in it", got)
	}
	if got != `{"reason":"OOMKilled"}` {
		t.Fatalf("reply = %q, want the answer — the filter must not swallow it", got)
	}
}

// TestShippedFakesEmitNoThinkingBlock is the other half of #372's decision, and
// the reason the filter above is a guard rather than a fix: the two exported
// fakes model a provider that does not reason.
//
// It is pinned rather than left implicit because the decision is reversible and
// somebody will reverse it. What changing the fakes costs is an embedder's test
// output, and this test is where the next person finds out that cost was
// weighed — the issue number in the failure message is the whole point.
func TestShippedFakesEmitNoThinkingBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    model.LLM
	}{
		{"echo", NewEchoModel("echo")},
		{"toolactor", NewToolActorModel("toolactor")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := reqWithResponseSchema(&genai.Schema{
				Type:       genai.TypeObject,
				Properties: map[string]*genai.Schema{"reason": {Type: genai.TypeString}},
				Required:   []string{"reason"},
			})
			for resp, err := range tc.m.GenerateContent(t.Context(), req, false) {
				if err != nil {
					t.Fatalf("GenerateContent: %v", err)
				}
				for _, p := range resp.Content.Parts {
					if p.Thought || len(p.ThoughtSignature) > 0 {
						t.Fatalf("fake emitted a thinking block (%+v); #372 decided it should not — "+
							"changing that is a change to a frozen package's observable output", p)
					}
				}
			}
		})
	}
}
