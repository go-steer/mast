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

// Command fan-out-fan-in is the forkable starter for workflow shape #1
// (fan-out-fan-in) from docs/workflow-scaffolding-design.md:
//
//	START → plan (FunctionNode, emits []probe)
//	          → check_workers (ParallelWorker over check_service, maxConcurrency=3)
//	              → collect_reports (JoinNode)
//	                  → summarize (FunctionNode, N → 1)
//
// The domain is a deliberately generic service-health sweep: the
// planner turns the input into a list of probes, workers check each
// service in parallel, the join collects the per-service reports, and
// the summarizer folds them into one fleet summary. Everything is a
// deterministic function node, so it runs fully offline with
// `go run .` — no model, no credentials, no network. Swap the worker
// for an AgentNode-wrapped Task specialist when the per-item work
// needs an LLM (pkg/graph shows the RunNode idiom for that).
//
// Self-contained on purpose (workflow-scaffolding-design.md, "Shapes
// are forkable starters, not demonstrations"): copy this directory,
// replace planner/worker/summarizer, and it is your code.
//
// HITL constraint (spike-2, ErrParallelHITLUnsupported): nothing in
// the parallel section may raise a RequestInputEvent. Operator
// escalation belongs AFTER the join — see the summarizer.
package main

import (
	"bufio"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"strings"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
)

// probe is one unit of fanned-out work. Fork point: carry whatever a
// worker needs to act independently (an ID, a URL, a log-file path).
type probe struct {
	Service string `json:"service"`
}

// report is one worker's result; the join hands the summarizer the
// full []report in planner order.
type report struct {
	Service   string `json:"service"`
	Status    string `json:"status"`
	LatencyMS int    `json:"latency_ms"`
}

const (
	workersName = "check_workers"
	// maxConcurrency caps in-flight workers INSIDE the ParallelWorker.
	// This is the lever that protects provider quotas / rate limits
	// when the worker calls an LLM or an API. Note it is per-node:
	// the graph-wide workflow.WithMaxConcurrency option is a different
	// knob (and workflowagent.New does not expose it), and NEITHER
	// governs dynamic RunNode children — see README.
	maxConcurrency = 3
)

var defaultServices = []string{"checkout", "payments", "search", "inventory", "auth"}

func main() {
	input := ""
	// Piped input replaces the default fleet: comma- or
	// newline-separated service names.
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		input = readAll(os.Stdin)
	}

	root, err := buildRoot()
	if err != nil {
		fatal(err)
	}
	r, err := newRunner(root)
	if err != nil {
		fatal(err)
	}
	if _, err := runSweep(context.Background(), r, "sweep-1", input, os.Stdout); err != nil {
		fatal(err)
	}
}

// buildRoot assembles the graph and wraps it as the runner's root
// agent (a workflowagent-wrapped graph is a valid root; spike-2 rule).
func buildRoot() (adkagent.Agent, error) {
	// Planner: input text → []probe. The emitted slice is what the
	// ParallelWorker fans out over, one worker activation per element.
	plan := workflow.NewFunctionNode("plan",
		func(_ adkagent.Context, input string) ([]probe, error) {
			services := parseServices(input)
			probes := make([]probe, len(services))
			for i, s := range services {
				probes[i] = probe{Service: s}
			}
			return probes, nil
		}, workflow.NodeConfig{})

	// Worker: one probe → one report. Deterministic stand-in for the
	// real per-item work (an HTTP health check, a log digest, a Task
	// specialist via RunNode). MUST NOT raise HITL: this node runs
	// inside parallel branches (ErrParallelHITLUnsupported).
	check := workflow.NewFunctionNode("check_service",
		func(_ adkagent.Context, p probe) (report, error) {
			status, latency := syntheticHealth(p.Service)
			return report{Service: p.Service, Status: status, LatencyMS: latency}, nil
		}, workflow.NodeConfig{})

	// ParallelWorker runs `check` once per []probe element with at
	// most maxConcurrency in flight, and aggregates the outputs into a
	// single list in input order. Branch isolation per item is the v2
	// default — no extra care needed.
	workers, err := workflow.NewParallelWorker(workersName, check, maxConcurrency, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("build parallel worker: %w", err)
	}

	// Join barrier: fires once, when every declared predecessor has
	// completed, and hands its successor a map keyed by predecessor
	// name. (v2.1.0 signature is NewJoinNode(name) — no NodeConfig.)
	join := workflow.NewJoinNode("collect_reports")

	// Summarizer: N reports → 1 fleet summary. THIS is where operator
	// escalation belongs — after the join the parallel section is
	// over, so a RequestInputEvent here (make it an
	// EmittingFunctionNode, ResumedInput-first) is legal, while the
	// same event inside `check` would fail the run.
	summarize := workflow.NewFunctionNode("summarize",
		func(_ adkagent.Context, in map[string]any) (string, error) {
			items, _ := in[workersName].([]any)
			reports := make([]report, 0, len(items))
			for _, item := range items {
				r, ok := item.(report)
				if !ok {
					return "", fmt.Errorf("summarize: unexpected worker output %T", item)
				}
				reports = append(reports, r)
			}
			return summarizeFleet(reports), nil
		}, workflow.NodeConfig{})

	b := workflow.NewEdgeBuilder()
	b.Add(workflow.Start, plan)
	b.Add(plan, workers)
	b.AddFanIn(join, workers)
	b.Add(join, summarize)

	return workflowagent.New(workflowagent.Config{
		Name:        "fleet_health_sweep",
		Description: "Fan-out-fan-in starter: plan probes, check services in parallel, join, summarize.",
		Edges:       b.Build(),
	})
}

func newRunner(root adkagent.Agent) (*runner.Runner, error) {
	return runner.New(runner.Config{
		AppName: "fan-out-fan-in-starter",
		Agent:   root,
		// In-memory sessions: no pause/resume in this starter. If you
		// add post-join HITL escalation, switch to
		// session/database.NewSessionService so the pause survives a
		// restart (see cmd/mast).
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
}

// runSweep drives one sweep through the graph and returns the terminal
// output (the fleet summary string).
func runSweep(ctx context.Context, r *runner.Runner, sessionID, input string, out io.Writer) (any, error) {
	fmt.Fprintf(out, "== %s: services=%v\n", sessionID, parseServices(input))
	msg := genai.NewContentFromText(input, genai.RoleUser)
	var final any
	for event, err := range r.Run(ctx, "starter-user", sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if err != nil {
			return nil, err
		}
		if event == nil || event.Output == nil {
			continue
		}
		final = event.Output
		fmt.Fprintf(out, "   [%s] output: %v\n", event.Author, event.Output)
	}
	return final, nil
}

// parseServices splits the input on commas/newlines; blank input means
// the default fleet.
func parseServices(input string) []string {
	fields := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == '\n'
	})
	var services []string
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			services = append(services, s)
		}
	}
	if len(services) == 0 {
		return defaultServices
	}
	return services
}

// syntheticHealth derives a stable pseudo-status from the service name
// so runs are deterministic offline. Replace with the real check.
func syntheticHealth(service string) (status string, latencyMS int) {
	h := fnv.New32a()
	h.Write([]byte(service))
	sum := h.Sum32()
	statuses := []string{"healthy", "healthy", "degraded", "unhealthy"}
	return statuses[sum%4], int(20 + sum%180)
}

// summarizeFleet is the reduce step: N reports → one summary. The
// escalation line is deliberately computed here, after the join, where
// a HITL gate would be legal.
func summarizeFleet(reports []report) string {
	counts := map[string]int{}
	var unhealthy []string
	var sb strings.Builder
	fmt.Fprintf(&sb, "fleet summary: %d services checked\n", len(reports))
	for _, r := range reports {
		counts[r.Status]++
		if r.Status == "unhealthy" {
			unhealthy = append(unhealthy, r.Service)
		}
		fmt.Fprintf(&sb, "  - %-10s %-9s %dms\n", r.Service, r.Status, r.LatencyMS)
	}
	fmt.Fprintf(&sb, "totals: %d healthy / %d degraded / %d unhealthy",
		counts["healthy"], counts["degraded"], counts["unhealthy"])
	if len(unhealthy) > 0 {
		fmt.Fprintf(&sb, "\nESCALATE: needs operator attention: %s", strings.Join(unhealthy, ", "))
	}
	return sb.String()
}

func readAll(r io.Reader) string {
	var sb strings.Builder
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		sb.WriteString(sc.Text())
		sb.WriteString("\n")
	}
	return sb.String()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fan-out-fan-in:", err)
	os.Exit(1)
}
