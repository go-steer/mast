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

// Command slim is the reference consumer for the slim-embed guarantee
// (docs/library-api-design.md, "Slim-embed guarantee"): a single-file
// host service embedding exactly one mast control loop in-process —
// a SingleTurn classifier feeding one Task specialist — with
// in-memory sessions and nothing else.
//
// What matters here is what this file IMPORTS, not what it serves:
// only the slim slice (pkg/agent, pkg/specialists, pkg/budget) plus
// ADK v2 and stdlib. No inject server, no metrics endpoint, no MCP,
// no pkg/graph or pkg/router dispatch, no pkg/config discovery. CI
// enforces that this dependency graph stays slim
// (scripts/check-slim-deps.sh).
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/specialists"
)

const appName = "slim-embed"

// sampleInputs stands in for whatever the host service would normally
// feed the loop (queue messages, webhook bodies, rows from a table).
// The `"reason"` field is what the classifier keys on.
var sampleInputs = []string{
	`INJECT {"reason":"CrashLoopBackOff","namespace":"payments","pod":"api-7f9c"}`,
	`INJECT {"reason":"OOMKilled","namespace":"search","pod":"indexer-0"}`,
	`INJECT {"reason":"FailedScheduling","namespace":"ml","pod":"trainer-2b"}`,
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "slim: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// The echo model is mast's offline fake (no credentials, no
	// network). Swap in a real model with one ADK import:
	//
	//	import "google.golang.org/adk/v2/model/gemini"
	//	llm, err := gemini.NewModel(ctx, "gemini-2.5-flash", &genai.ClientConfig{})
	llm := mastagent.NewEchoModel("slim-echo")

	// Specs are registered programmatically — no .agents/ directory,
	// no file discovery, no pkg/config. The same Specs could instead
	// be loaded from .tmpl files via specialists.LoadDir.
	classifier, err := specialists.Build(specialists.Spec{
		Name:        "incident_classifier",
		Description: "Classifies an incident envelope into a failure mode.",
		Mode:        specialists.ModeSingleTurn,
		Instruction: "Reply with the single failure-mode keyword for the incident envelope.",
	}, specialists.BuildOptions{Model: llm})
	if err != nil {
		return fmt.Errorf("build classifier: %w", err)
	}

	triager, err := specialists.Build(specialists.Spec{
		Name:        "incident_triager",
		Description: "Diagnoses a classified incident and returns a digest.",
		Mode:        specialists.ModeTask,
		Instruction: "Diagnose the incident and finish with a one-line digest.",
	}, specialists.BuildOptions{Model: llm})
	if err != nil {
		return fmt.Errorf("build triager: %w", err)
	}

	root, err := buildLoop(classifier, triager)
	if err != nil {
		return fmt.Errorf("build control loop: %w", err)
	}

	// In-memory sessions: no durability, no database driver.
	// Durability later is an additive swap — ADK's session/database
	// service over SQLite in the same runner.Config field.
	r, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return fmt.Errorf("construct runner: %w", err)
	}

	// Optional slim-slice governance: a per-process budget meter. No
	// metrics endpoint, no exporters — just an in-process ceiling.
	meter := budget.NewMeter(budget.Limits{RatePer1K: 0.05, MaxCostUSD: 1.00})

	for i, input := range sampleInputs {
		verdict, digest, err := runTurn(ctx, r, fmt.Sprintf("incident-%d", i), input, meter)
		if err != nil {
			return fmt.Errorf("triage input %d: %w", i, err)
		}
		fmt.Printf("input %d: classified=%q digest=%q\n", i, verdict, digest)
	}

	tokens, cost, calls := meter.Snapshot()
	fmt.Printf("budget: %d tokens, $%.4f, %d model calls\n", tokens, cost, calls)
	return nil
}

// buildLoop assembles the control loop as a two-node ADK workflow
// graph — START → classify → triage — wrapped as a runnable root
// agent. Stock ADK v2 primitives; no mast dispatch packages involved.
//
// Task-mode agents cannot be static graph nodes, so the triage step
// is a DynamicNode that composes the specialist's brief (classifier
// verdict + original envelope) and invokes the specialist's AgentNode
// via workflow.RunNode — the sanctioned dynamic-invocation pattern.
func buildLoop(classifier, triager adkagent.Agent) (adkagent.Agent, error) {
	classifyNode, err := workflow.NewAgentNode(classifier, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("wrap classifier: %w", err)
	}
	triageAgentNode, err := workflow.NewAgentNode(triager, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("wrap triager: %w", err)
	}
	triageNode := workflow.NewDynamicNode[any, any]("run_triage",
		func(ctx adkagent.Context, verdict any, _ func(*session.Event) error) (any, error) {
			brief := fmt.Sprintf("Failure mode: %v\n%s", verdict, userText(ctx))
			return workflow.RunNode[any](ctx, triageAgentNode, brief)
		}, workflow.NodeConfig{})

	return workflowagent.New(workflowagent.Config{
		Name:        "slim_triage",
		Description: "Minimal classifier-to-specialist triage loop.",
		Edges:       workflow.Chain(workflow.Start, classifyNode, triageNode),
		SubAgents:   []adkagent.Agent{classifier, triager},
	})
}

// runTurn drives one turn through the ADK runner and returns the
// classifier's verdict (the last plain-text model reply seen) and the
// turn's final output (the Task-mode finish_task result if present,
// otherwise that same last text).
func runTurn(ctx context.Context, r *runner.Runner, sessionID, input string, meter *budget.Meter) (verdict, out string, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	msg := genai.NewContentFromText(input, genai.RoleUser)
	for event, err := range r.Run(ctx, "slim-host", sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if err != nil {
			return "", "", err
		}
		if berr := meter.Observe(event); berr != nil {
			cancel()
			return "", "", fmt.Errorf("budget exceeded: %w", berr)
		}
		if event == nil {
			continue
		}
		if event.Output != nil {
			out = fmt.Sprintf("%v", event.Output)
			continue
		}
		if event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part != nil && part.Text != "" {
				verdict = part.Text
				out = part.Text
			}
		}
	}
	return verdict, out, nil
}

// userText returns the text of the turn's user message (the incident
// envelope fed to the loop).
func userText(ctx adkagent.Context) string {
	uc := ctx.UserContent()
	if uc == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range uc.Parts {
		if p != nil {
			sb.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(sb.String())
}
