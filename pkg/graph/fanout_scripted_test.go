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

package graph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"

	"github.com/go-steer/mast/pkg/providers/mock"
	"github.com/go-steer/mast/pkg/specialists"
)

// TestFanoutReplaysOneRecordingPerBranch is the end-to-end half of
// #229, and the reason it lives here rather than in pkg/providers/mock:
// the provider's per-branch rule is only worth anything if a real
// branch tag actually reaches the model, and the only thing that
// proves that is the shape that produced the defect — parallelagent
// running the roster at once against the one instance
// internal/compose hands every specialist when the root model is an
// offline fake.
//
// The recording is ONE turn. That is the assertion: four consumers
// (three analysts and synthesis), one recorded turn, and a run that
// completes with every analyst reporting. Against a single shared
// cursor this cannot pass at all — the first consumer takes the turn
// and the rest get "script exhausted", with which ones depending on
// goroutine scheduling.
func TestFanoutReplaysOneRecordingPerBranch(t *testing.T) {
	path := writeRecording(t, finishTurn("reported"))

	// Repeated because the defect is nondeterministic: one green run
	// against a shared cursor was always possible, a green sweep was
	// not.
	for range 5 {
		msg := runScriptedFanout(t, path, countingAnalyst, "get_k8s_resource")
		if !strings.Contains(msg, "3 of 3 analysts") {
			t.Fatalf("gate message = %q, want 3 of 3 analysts", msg)
		}
	}
}

// TestFanoutScriptedBranchesGetTheWholeRecording pins the other
// direction: a branch walks its own copy end to end, so a two-turn
// recording is two turns FOR EACH consumer rather than two turns
// shared out among them. Every specialist here takes two model calls
// (a tool call, then finish_task), which one cursor could serve for
// exactly one of them.
func TestFanoutScriptedBranchesGetTheWholeRecording(t *testing.T) {
	path := writeRecording(t, lookTurn(), finishTurn("reported"))
	msg := runScriptedFanout(t, path, toolAnalystOn, "look")
	if !strings.Contains(msg, "3 of 3 analysts") {
		t.Fatalf("gate message = %q, want 3 of 3 analysts", msg)
	}
}

// runScriptedFanout builds a three-analyst fan-out whose whole roster
// shares one scripted LLM — the collapse internal/compose performs for
// an offline fake — runs it to the synthesis gate, and returns the
// operator prompt.
func runScriptedFanout(t *testing.T, path string, build func(*testing.T, string, model.LLM) adkagent.Agent, tools ...string) string {
	t.Helper()
	llm, err := mock.NewScripted(path, false)
	if err != nil {
		t.Fatalf("NewScripted: %v", err)
	}
	b := catalogBundle(tools...)
	b.HITL.RequireApproval = true

	var analysts []Analyst
	for _, name := range []string{"alpha", "beta", "gamma"} {
		analysts = append(analysts, Analyst{
			Name:  name,
			Agent: build(t, name, llm),
			Tools: readOnly(tools...),
		})
	}
	root, err := BuildFanout(FanoutConfig{
		Bundle:    b,
		Analysts:  analysts,
		Synthesis: Specialist{Agent: build(t, SynthesisName, llm)},
		Mutating:  catalogPredicate(b),
	})
	if err != nil {
		t.Fatalf("BuildFanout: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "fanout-scripted", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	var pending *session.RequestInput
	for ev, err := range r.Run(context.Background(), "op", "s1", genai.NewContentFromText("audit prod", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev.RequestedInput != nil {
			pending = ev.RequestedInput
		}
	}
	if pending == nil {
		t.Fatal("scripted fan-out finished without raising the synthesis gate")
	}
	return pending.Message
}

// writeRecording writes turns as the recorder does — one
// json.Encoder.Encode per line — into the test's temp dir.
func writeRecording(t *testing.T, turns ...mock.RecordedTurn) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create recording: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, turn := range turns {
		if err := enc.Encode(turn); err != nil {
			t.Fatalf("encode turn: %v", err)
		}
	}
	return path
}

func recordedTurn(parts ...*genai.Part) mock.RecordedTurn {
	return mock.RecordedTurn{
		Responses: []*model.LLMResponse{{
			Content:       &genai.Content{Role: genai.RoleModel, Parts: parts},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 1},
			TurnComplete:  true,
			FinishReason:  genai.FinishReasonStop,
		}},
	}
}

func finishTurn(title string) mock.RecordedTurn {
	return recordedTurn(genai.NewPartFromFunctionCall("finish_task", map[string]any{"title": title}))
}

func lookTurn() mock.RecordedTurn {
	return recordedTurn(genai.NewPartFromFunctionCall("look", map[string]any{}))
}

// toolAnalystOn is a Task specialist holding the `look` tool on the
// given model, so a two-turn recording (call, then finish) is a legal
// replay for it.
func toolAnalystOn(t *testing.T, name string, m model.LLM) adkagent.Agent {
	t.Helper()
	look, err := functiontool.New(functiontool.Config{
		Name:        "look",
		Description: "read something from the cluster",
	}, func(adkagent.Context, struct{}) (map[string]any, error) {
		return map[string]any{"seen": "a pod"}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	a, err := specialists.Build(specialists.Spec{
		Name: name, Description: name, Mode: specialists.ModeTask,
		Instruction: "look, then report", OutputSchema: titleSchema,
	}, specialists.BuildOptions{Model: m, Tools: []tool.Tool{look}})
	if err != nil {
		t.Fatalf("build %q: %v", name, err)
	}
	return a
}
