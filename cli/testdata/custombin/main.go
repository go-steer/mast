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

// Command custombin is the custom main.go cli_test.go builds and runs: a
// mast binary with one model and one tool of its own. It lives under
// testdata so `go build ./...` leaves it alone.
package main

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/mast/cli"
)

func main() {
	applyChange, err := functiontool.New(functiontool.Config{
		Name:        "apply_change",
		Description: "Applies a change. The test asserts mast parks this call rather than running it.",
	}, func(_ agent.Context, args struct{}) (map[string]string, error) {
		// Leaves a mark if it ever runs, which under the default
		// on_mutation policy it must not without an approval.
		if dir := os.Getenv("CUSTOMBIN_MARK_DIR"); dir != "" {
			_ = os.WriteFile(filepath.Join(dir, "apply_change.ran"), nil, 0o600)
		}
		return map[string]string{"status": "applied"}, nil
	})
	if err != nil {
		panic(err)
	}
	os.Exit(cli.Main(context.Background(), os.Args[1:],
		cli.WithModels(func(name string) (model.LLM, bool) {
			if name == "acme-echo" {
				return acmeEcho{}, true
			}
			return nil, false
		}),
		cli.WithTools("tickets", applyChange),
	))
}

// acmeEcho answers every request with "acme says: " and the last user
// text, so the test can tell its answer from any model mast ships.
type acmeEcho struct{}

func (acmeEcho) Name() string { return "acme-echo" }

func (acmeEcho) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	last := ""
	for _, c := range req.Contents {
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				last = p.Text
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: genai.NewContentFromText("acme says: "+strings.TrimSpace(last), genai.RoleModel),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 10, CandidatesTokenCount: 5, TotalTokenCount: 15,
			},
		}, nil)
	}
}
