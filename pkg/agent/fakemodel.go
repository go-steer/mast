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

import (
	"context"
	"fmt"
	"iter"
	"math"
	"regexp"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
)

// NewEchoModel returns a model.LLM that responds to any request with a
// canned "acknowledged" reply summarising the last user message. It
// makes no network calls and requires no credentials, so it lets the
// runtime wiring be smoke-tested end-to-end without ADK-model
// dependencies.
//
// This is a spike-only helper; it will be replaced by
// google.golang.org/adk/v2/model/gemini.NewModel once the real Gemini
// wiring lands (spike step 6).
func NewEchoModel(name string) model.LLM {
	return &echoModel{name: name}
}

type echoModel struct {
	name string
}

func (m *echoModel) Name() string { return m.name }

// reasonRe extracts the k8s event reason from an INJECT envelope in
// the prompt, letting the fake behave like a triage classifier without
// a real LLM.
var reasonRe = regexp.MustCompile(`"reason"\s*:\s*"(\w+)"`)

func (m *echoModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		last := lastUserText(req)

		// Synthesize plausible usage so budget metering can be
		// exercised offline (real models populate this the same way).
		usage := &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     int32(min(len(last)/4, math.MaxInt32)), // #nosec G115 -- clamped to MaxInt32
			CandidatesTokenCount: 16,
		}
		usage.TotalTokenCount = usage.PromptTokenCount + usage.CandidatesTokenCount

		// Task-mode agents auto-install finish_task and terminate by
		// calling it; when it's declared, play along so Task
		// specialists complete deterministically offline.
		if _, ok := req.Tools["finish_task"]; ok {
			resp := &model.LLMResponse{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{genai.NewPartFromFunctionCall("finish_task", map[string]any{
						"result": fmt.Sprintf("[echo triage] diagnosed from envelope: %s", firstMatch(reasonRe, last)),
					})},
				},
				UsageMetadata: usage,
				TurnComplete:  true,
				FinishReason:  genai.FinishReasonStop,
			}
			yield(resp, nil)
			return
		}

		// Toolless one-shot request carrying an INJECT envelope: act as
		// the classifier and reply with the bare reason word so
		// StringRoute matching is exercised offline.
		reply := fmt.Sprintf("[echo] acknowledged: %s", last)
		if reason := firstMatch(reasonRe, last); reason != "" {
			reply = reason
		}
		resp := &model.LLMResponse{
			Content:       genai.NewContentFromText(reply, genai.RoleModel),
			UsageMetadata: usage,
			TurnComplete:  true,
			FinishReason:  genai.FinishReasonStop,
		}
		yield(resp, nil)
	}
}

func firstMatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	return ""
}

func lastUserText(req *model.LLMRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		for _, part := range c.Parts {
			if part != nil && part.Text != "" {
				return part.Text
			}
		}
	}
	return ""
}
