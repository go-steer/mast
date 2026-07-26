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

// Command llm-as-router is the forkable starter for workflow shape #7
// (LLM-as-router) from docs/workflow-scaffolding-design.md:
//
//	START → classify (SingleTurn AgentNode) → route_by_category
//	          ├─ StringRoute("billing")  → handle_billing  (DynamicNode → Task specialist)
//	          ├─ StringRoute("outage")   → handle_outage
//	          ├─ StringRoute("account")  → handle_account
//	          └─ Default                 → handle_general
//
// The domain here is deliberately generic — support-ticket routing —
// so the shape is easy to gut and refill. The GKE-flavoured instance
// of the same shape lives in pkg/graph (the triage anchor workload);
// this file intentionally does NOT import it. Starters are
// self-contained by design (workflow-scaffolding-design.md, "Shapes
// are forkable starters, not demonstrations"): copy this directory,
// replace the categories and the fake models, and it is your code.
//
// Runs fully offline with `go run .` — the classifier and the
// specialists are deterministic in-process fakes; no credentials, no
// network.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"iter"
	"os"
	"strings"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

// category is the fork point: one entry per route the classifier can
// pick. Swap these for your own domains (incident types, document
// kinds, intents, ...). The keywords only drive the offline fake
// classifier; a real model replaces them with actual judgment.
type category struct {
	name        string
	keywords    []string
	instruction string
	resolution  string // canned result the offline fake specialist returns
}

var categories = []category{
	{
		name:        "billing",
		keywords:    []string{"refund", "charge", "invoice", "billing", "payment"},
		instruction: "You are the billing specialist. Diagnose the billing issue, verify the customer's recent invoices, and call finish_task with a concise resolution.",
		resolution:  "duplicate charge confirmed; refund issued and invoice corrected",
	},
	{
		name:        "outage",
		keywords:    []string{"down", "outage", "500", "unreachable", "timeout", "crash"},
		instruction: "You are the outage specialist. Correlate the report with known incidents and call finish_task with status and ETA.",
		resolution:  "matched to incident INC-1042; mitigation in progress, ETA 30m",
	},
	{
		name:        "account",
		keywords:    []string{"password", "login", "locked", "2fa", "account"},
		instruction: "You are the account specialist. Resolve access problems safely and call finish_task with the action taken.",
		resolution:  "account lockout cleared; password-reset link sent to the address on file",
	},
}

// fallback handles everything the classifier can't map to a category —
// the workflow.Default route. Every router graph needs one.
var fallback = category{
	name:        "general",
	instruction: "You are the general support agent. Handle tickets no specialist claimed and call finish_task with a next step.",
	resolution:  "no specialist matched; ticket queued for a human agent with full context attached",
}

func main() {
	tickets := []string{
		"I was double-charged on my last invoice, please refund the duplicate.",
		"Your API has been returning 500s since 09:00 UTC and our dashboard is unreachable.",
		"How do I export my project data to CSV?",
	}
	// Piped input replaces the samples: one ticket per line.
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		if piped := readLines(os.Stdin); len(piped) > 0 {
			tickets = piped
		}
	}

	root, err := buildRoot()
	if err != nil {
		fatal(err)
	}
	r, err := newRunner(root)
	if err != nil {
		fatal(err)
	}

	ctx := context.Background()
	for i, ticket := range tickets {
		// One session per ticket: routing decisions should not share
		// history (mirrors the per-incident-session rule from spike 2).
		if _, err := runTicket(ctx, r, fmt.Sprintf("ticket-%d", i+1), ticket, os.Stdout); err != nil {
			fatal(err)
		}
	}
}

// buildRoot assembles the graph and wraps it as the runner's root
// agent. Spike-2 rule: a workflowagent-wrapped graph IS a valid root —
// no coordinator agent above it (the runner's Chat-mode restriction
// applies only when the root itself is an LlmAgent).
func buildRoot() (adkagent.Agent, error) {
	classifier, err := mastagent.NewSingleTurnAgent(mastagent.SingleTurnAgentConfig{
		Name:        "TicketClassifier",
		Description: "Classifies a support ticket into a routing category.",
		Instruction: "Classify the support ticket into exactly one of: billing, outage, account, unknown. Reply with the single category word and nothing else.",
		Model:       classifierModel{},
	})
	if err != nil {
		return nil, fmt.Errorf("build classifier: %w", err)
	}
	classifyNode, err := workflow.NewAgentNode(classifier, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("wrap classifier: %w", err)
	}

	// Normalize the classifier's free-text reply into a route key and
	// emit it as Event.Routes. Anything unrecognized falls through to
	// the Default edge (no route matches it).
	routeNode := workflow.NewEmittingFunctionNode("route_by_category",
		func(ctx adkagent.Context, input any, emit func(*session.Event) error) (any, error) {
			key := strings.ToLower(strings.TrimSpace(fmt.Sprint(input)))
			key = strings.TrimRight(key, ".")
			ev := session.NewEvent(ctx, ctx.InvocationID())
			ev.Routes = []string{key}
			if err := emit(ev); err != nil {
				return nil, err
			}
			return nil, nil
		}, workflow.NodeConfig{})

	edges := workflow.Chain(workflow.Start, classifyNode, routeNode)
	subAgents := []adkagent.Agent{classifier}

	for _, c := range append(append([]category{}, categories...), fallback) {
		specialist, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
			Name:        c.name,
			Description: "Handles " + c.name + " tickets.",
			Instruction: c.instruction,
			Model:       specialistModel{category: c.name, resolution: c.resolution},
		})
		if err != nil {
			return nil, fmt.Errorf("build specialist %q: %w", c.name, err)
		}
		spNode, err := workflow.NewAgentNode(specialist, workflow.NodeConfig{})
		if err != nil {
			return nil, fmt.Errorf("wrap specialist %q: %w", c.name, err)
		}

		// Task-mode agents can't be static graph nodes; each routed
		// branch is a DynamicNode invoking the specialist via RunNode.
		// The route node forwards only the route key, so the body
		// re-reads the original ticket from the user content.
		//
		// If you add an approval gate (HITL) here, the body MUST be
		// ResumedInput-first: check ctx.ResumedInput(id) before any
		// RunNode call and stash the specialist's result in session
		// state before interrupting — dynamic-node bodies re-execute on
		// resume and RunNode does not cache child results across the
		// pause turn. pkg/graph carries the full worked gate.
		handle := workflow.NewDynamicNode[any, any]("handle_"+c.name,
			func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (any, error) {
				return workflow.RunNode[any](ctx, spNode, ticketText(ctx))
			}, workflow.NodeConfig{})

		var route workflow.Route = workflow.StringRoute(c.name)
		if c.name == fallback.name {
			route = workflow.Default
		}
		edges = append(edges, workflow.Edge{From: routeNode, To: handle, Route: route})
		subAgents = append(subAgents, specialist)
	}

	return workflowagent.New(workflowagent.Config{
		Name:        "support_router",
		Description: "LLM-as-router starter: classify a support ticket, dispatch to the matching specialist.",
		Edges:       edges,
		// Wrapped agents are registered so the runner can resolve
		// event authorship for the events they emit.
		SubAgents: subAgents,
	})
}

func newRunner(root adkagent.Agent) (*runner.Runner, error) {
	return runner.New(runner.Config{
		AppName: "llm-as-router-starter",
		Agent:   root,
		// In-memory sessions: this starter has no pause/resume, so
		// nothing needs to survive a restart. For durable HITL, swap in
		// session/database.NewSessionService (see pkg/graph + cmd/mast).
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
}

// runTicket drives one ticket through the graph and returns the
// terminal node output (the specialist's finish_task result).
func runTicket(ctx context.Context, r *runner.Runner, sessionID, ticket string, out io.Writer) (any, error) {
	fmt.Fprintf(out, "== %s: %s\n", sessionID, ticket)
	msg := genai.NewContentFromText(ticket, genai.RoleUser)
	var final any
	for event, err := range r.Run(ctx, "starter-user", sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if err != nil {
			return nil, err
		}
		if event == nil {
			continue
		}
		if text := eventText(event); text != "" {
			fmt.Fprintf(out, "   [%s] %s\n", event.Author, text)
		}
		if event.Output != nil {
			final = event.Output
			fmt.Fprintf(out, "   [%s] output: %v\n", event.Author, event.Output)
		}
	}
	return final, nil
}

// --- offline fake models -------------------------------------------
//
// Both fakes are the seams where a real model goes. Replace them with
// e.g. gemini.NewModel(ctx, "gemini-2.5-flash", &genai.ClientConfig{})
// and delete the keyword table; nothing else in the graph changes.

// classifierModel stands in for the SingleTurn routing model: it
// keyword-matches the ticket against the category table and replies
// with the bare category word, exactly as the instruction asks.
type classifierModel struct{}

func (classifierModel) Name() string { return "fake-classifier" }

func (classifierModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		ticket := strings.ToLower(lastUserText(req))
		label := "unknown"
	scan:
		for _, c := range categories {
			for _, kw := range c.keywords {
				if strings.Contains(ticket, kw) {
					label = c.name
					break scan
				}
			}
		}
		yield(textResponse(label), nil)
	}
}

// specialistModel stands in for a Task-mode specialist's model: Task
// agents auto-install finish_task and terminate by calling it, so the
// fake plays finish_task with a canned per-category resolution.
type specialistModel struct {
	category   string
	resolution string
}

func (m specialistModel) Name() string { return "fake-" + m.category }

func (m specialistModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		result := fmt.Sprintf("[%s] %s", m.category, m.resolution)
		if _, ok := req.Tools["finish_task"]; ok {
			yield(&model.LLMResponse{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{genai.NewPartFromFunctionCall("finish_task", map[string]any{
						"result": result,
					})},
				},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}, nil)
			return
		}
		yield(textResponse(result), nil)
	}
}

func textResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      genai.NewContentFromText(text, genai.RoleModel),
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
}

// --- small helpers --------------------------------------------------

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

// ticketText re-reads the original user message; the route node only
// forwards the route key, not the ticket body.
func ticketText(ctx adkagent.Context) string {
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

func eventText(event *session.Event) string {
	if event.Content == nil {
		return ""
	}
	for _, part := range event.Content.Parts {
		if part == nil {
			continue
		}
		if part.Text != "" {
			return part.Text
		}
		if part.FunctionCall != nil {
			return "function_call:" + part.FunctionCall.Name
		}
		if part.FunctionResponse != nil {
			return "function_response:" + part.FunctionResponse.Name
		}
	}
	return ""
}

func readLines(r io.Reader) []string {
	var lines []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "llm-as-router:", err)
	os.Exit(1)
}
