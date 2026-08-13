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

package judge

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/mast/internal/evals"
)

// scriptedModel answers a fixed sequence of turns. It is not a stand-in
// for the real model — the tier's whole point is that a scripted model
// cannot choose — but it lets the plumbing (tool wiring, session
// recording, trace extraction, scoring) run in CI without credentials.
type scriptedModel struct {
	mu    sync.Mutex
	turns []func(req *adkmodel.LLMRequest) *adkmodel.LLMResponse
	at    int
}

func (m *scriptedModel) Name() string { return "judge-test-script" }

func (m *scriptedModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		i := m.at
		m.at++
		m.mu.Unlock()
		if i >= len(m.turns) {
			// Falling off the script means the composed shape asked for
			// more turns than the fixture expects. Ending the turn is
			// wrong-but-quiet; the test asserts on what was recorded.
			yield(&adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText("script exhausted")}},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}, nil)
			return
		}
		yield(m.turns[i](req), nil)
	}
}

func call(name string, args map[string]any) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)}},
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
}

// TestRig_RunsAScenarioEndToEnd is the offline plumbing check: a
// scripted model reads one tool and reports, and the rig must come back
// with the tool call recorded, the report as final text, and the
// deterministic metrics scored.
func TestRig_RunsAScenarioEndToEnd(t *testing.T) {
	ds := loadCorpus(t)
	tbl := loadIntents(t)
	fx, err := Fixtures(ds, loadOverrides(t))
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}

	var sc evals.Scenario
	for _, s := range ds.Scenarios {
		if s.ID == "LC-01-crashloopbackoff" {
			sc = s
		}
	}
	if sc.ID == "" {
		t.Fatal("LC-01 not in the corpus")
	}

	const report = "CRITICAL: api-server-7d8f9c-xkp2v in production is crashlooping because " +
		"the Secret api-server-secrets has no DATABASE_URL key. Recommended action: add the key."

	var sawReading string
	m := &scriptedModel{turns: []func(*adkmodel.LLMRequest) *adkmodel.LLMResponse{
		func(*adkmodel.LLMRequest) *adkmodel.LLMResponse {
			return call("k8s_triage_workload", map[string]any{"scope": "production/api-server"})
		},
		func(req *adkmodel.LLMRequest) *adkmodel.LLMResponse {
			// Capture what the tool actually returned to the model, so
			// the test can assert the fixture reached it.
			for _, c := range req.Contents {
				for _, p := range c.Parts {
					if p.FunctionResponse != nil {
						if r, ok := p.FunctionResponse.Response["reading"].(string); ok {
							sawReading = r
						}
					}
				}
			}
			// Chat mode: the agent's answer is model text, not a
			// finish_task argument.
			return &adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(report)}},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}
		},
	}}

	rig, err := NewRig(tbl, fx, m, t.TempDir())
	if err != nil {
		t.Fatalf("NewRig: %v", err)
	}
	out, err := rig.Run(context.Background(), sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(out.Tools) != 1 || out.Tools[0] != "k8s_triage_workload" {
		t.Errorf("tools called = %v, want one k8s_triage_workload", out.Tools)
	}
	if !strings.Contains(sawReading, "DATABASE_URL not set") {
		t.Errorf("the fixture reading did not reach the model:\n%s", sawReading)
	}
	if !strings.Contains(out.Response, "CRITICAL") {
		t.Errorf("final response did not carry the report: %q", out.Response)
	}

	got := map[string]float64{}
	for _, res := range out.Results {
		got[res.Metric] = res.Score
	}
	if got[evals.MetricSeverityAccuracy] != 1 {
		t.Errorf("severity_accuracy = %v, want 1 (report and ground truth are both CRITICAL): %v",
			got[evals.MetricSeverityAccuracy], out.summarize())
	}
	if got[evals.MetricIntentCoverage] == 0 {
		t.Errorf("intent_coverage = 0 despite a triage call: %v", out.summarize())
	}
	t.Logf("%s: %s (ceiling %.2f)", out.ID, out.summarize(), out.Ceiling)
}

// TestRig_CeilingExposesTheWriteToolGap pins the one scenario a
// read-only surface cannot fully cover. Reported, never folded into the
// score: LC-13 expects kubectl_rollback_deployment, and lookout excludes
// write tools by design.
func TestRig_CeilingExposesTheWriteToolGap(t *testing.T) {
	ds := loadCorpus(t)
	tbl := loadIntents(t)
	fx, err := Fixtures(ds, loadOverrides(t))
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}
	rig, err := NewRig(tbl, fx, &scriptedModel{}, t.TempDir())
	if err != nil {
		t.Fatalf("NewRig: %v", err)
	}

	var capped []string
	for _, sc := range ds.Scenarios {
		if c := rig.ceiling(sc); c < 1 {
			capped = append(capped, sc.ID)
		}
	}
	if len(capped) != 1 || capped[0] != "LC-13-rollback-needed-after-bad" {
		t.Errorf("scenarios capped below 1.0 = %v, want only LC-13", capped)
	}
}

// TestBuildTools_EveryShapedToolIsDescribed keeps the model from being
// asked to choose between tools it has no description for.
func TestBuildTools_EveryShapedToolIsDescribed(t *testing.T) {
	for name := range toolShape {
		if _, ok := toolDescription[name]; !ok {
			t.Errorf("tool %q has a shape but no description", name)
		}
	}
	for name := range toolDescription {
		if _, ok := toolShape[name]; !ok {
			t.Errorf("tool %q has a description but no shape", name)
		}
	}
}
