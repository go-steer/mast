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
	"context"
	"fmt"
	"iter"
	"regexp"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// triageFake is this example's offline model: no credentials, no
// network. It is written out here rather than imported because a starter
// is copied out of this repository, and mast's own fakes are internal to
// it. Swap it for a real model in run().
//
// It plays both parts of the loop well enough to show the wiring: as the
// classifier (no tools offered) it answers with the incident's reason, and
// as the Task specialist (finish_task offered) it finishes with a digest.
// Every reply reports usage, so the budget meter has something to fold.
type triageFake struct{}

var reasonRe = regexp.MustCompile(`"reason"\s*:\s*"(\w+)"`)

func (triageFake) Name() string { return "slim-fake" }

func (triageFake) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	reason := "Unknown"
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if m := reasonRe.FindStringSubmatch(p.Text); m != nil {
				reason = m[1]
			}
		}
	}
	usage := &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 120, CandidatesTokenCount: 16, TotalTokenCount: 136}
	content := genai.NewContentFromText(reason, genai.RoleModel)
	if _, ok := req.Tools["finish_task"]; ok {
		content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			genai.NewPartFromFunctionCall("finish_task", map[string]any{
				"result": fmt.Sprintf("diagnosed %s from the envelope", reason),
			}),
		}}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:       content,
			UsageMetadata: usage,
			TurnComplete:  true,
			FinishReason:  genai.FinishReasonStop,
		}, nil)
	}
}
