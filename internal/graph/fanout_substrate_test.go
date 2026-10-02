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
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/agent/workflowagents/parallelagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
)

// chatter is a non-output event a branch emits before it returns its
// finding. Whether it reaches the runner is the difference between the
// two parallel substrates ADK offers, and it decided which one the
// fan-out is built on.
const chatter = "branch-internal chatter"

// The fan-out shape rests on two ADK properties that are not written
// down in ADK's own docs, and both are load-bearing for BuildFanout.
// They are pinned against the substrate rather than against mast's
// wiring, so an ADK upgrade that changes either one fails on the
// property itself instead of somewhere downstream in a UAT leg.
//
// The two subtests run the SAME branch body under the two parallel
// substrates and get opposite answers. That contrast is the reason
// BuildFanout uses parallelagent, so it is kept as a test rather than
// as a paragraph in a design doc.
func TestFanoutSubstrate(t *testing.T) {
	// Property 1: workflow.ParallelWorker suppresses everything a
	// branch emits that is not an output event — runWrappedOnce keeps
	// only extractOutput hits and yields nothing upward. Nothing
	// suppressed reaches the runner, and only the runner appends to the
	// session, so a branch under ParallelWorker cannot see its own tool
	// results on the next model call: an LLM agent's working memory IS
	// the session event list (internal/llminternal/contents_processor.go
	// rebuilds req.Contents from Session().Events().All()). Every
	// tool-using analyst would loop until something cancelled it.
	t.Run("parallel_worker_suppresses_branch_events", func(t *testing.T) {
		transcript, ran := runSubstrate(t, parallelWorkerRoot)
		if len(ran) != 2 {
			t.Fatalf("branch body ran %d times (%v), want 2", len(ran), ran)
		}
		if strings.Contains(transcript, chatter) {
			t.Fatal("a branch event escaped ParallelWorker; if this is now true the substrate changed and BuildFanout could go back to the simpler shape")
		}
	})

	// Property 2: parallelagent funnels every sub-agent event up
	// through resultsChan and waits on ackChan for the caller to finish
	// processing it ("including session append"), so branch events do
	// reach the runner and do land in the session. That is what makes a
	// tool-using analyst possible inside a branch, what lets
	// per-specialist budget scopes bite (they bucket on
	// session.Event.Author), and what puts a branch's work in the event
	// log where crash recovery can see it.
	//
	// Both subtests dispatch through workflow.RunNode from inside a
	// DynamicNode, which is legal only because DynamicNode.Run installs
	// its own agent.DynamicSubScheduler unconditionally — the scheduler
	// hands every node it activates a nil one.
	t.Run("parallelagent_delivers_branch_events", func(t *testing.T) {
		transcript, ran := runSubstrate(t, parallelAgentRoot)
		if len(ran) != 2 {
			t.Fatalf("branch body ran %d times (%v), want 2", len(ran), ran)
		}
		for _, name := range []string{"a", "b"} {
			if !strings.Contains(transcript, chatter+" "+name) {
				t.Fatalf("branch %q emitted an event that never reached the runner; an analyst that cannot get its own events into the session cannot use a tool", name)
			}
		}
	})
}

// branchBody is the node each branch dispatches to: it emits one
// non-output event, then returns its finding as the branch output.
func branchBody(name string, ran *[]string, mu *sync.Mutex) workflow.Node {
	return workflow.NewEmittingFunctionNode[any, string]("analyst_"+name,
		func(ctx adkagent.Context, _ any, emit func(*session.Event) error) (string, error) {
			mu.Lock()
			*ran = append(*ran, name)
			mu.Unlock()
			ev := session.NewEvent(ctx, ctx.InvocationID())
			ev.Content = genai.NewContentFromText(chatter+" "+name, genai.RoleModel)
			if err := emit(ev); err != nil {
				return "", err
			}
			return "finding-" + name, nil
		}, workflow.NodeConfig{})
}

// parallelWorkerRoot is the shape BuildFanout used to have: one wrapped
// dispatching body, ParallelWorker over the roster as items.
func parallelWorkerRoot(t *testing.T, ran *[]string, mu *sync.Mutex) adkagent.Agent {
	t.Helper()
	bodies := map[string]workflow.Node{
		"a": branchBody("a", ran, mu),
		"b": branchBody("b", ran, mu),
	}
	analyze := workflow.NewDynamicNode[string, any]("analyze",
		func(ctx adkagent.Context, item string, _ func(*session.Event) error) (any, error) {
			return workflow.RunNode[any](ctx, bodies[item], item, workflow.WithUseAsOutput())
		}, workflow.NodeConfig{})
	worker, err := workflow.NewParallelWorker("fan_out", analyze, 2, workflow.NodeConfig{})
	if err != nil {
		t.Fatalf("NewParallelWorker: %v", err)
	}
	plan := workflow.NewFunctionNode[any, []any]("plan",
		func(adkagent.Context, any) ([]any, error) { return []any{"a", "b"}, nil },
		workflow.NodeConfig{})
	root, err := workflowagent.New(workflowagent.Config{
		Name:  "worker_probe",
		Edges: workflow.Chain(workflow.Start, plan, worker),
	})
	if err != nil {
		t.Fatalf("workflowagent.New: %v", err)
	}
	return root
}

// parallelAgentRoot is the shape BuildFanout has now: one workflowagent
// per branch, all of them sub-agents of a parallelagent.
func parallelAgentRoot(t *testing.T, ran *[]string, mu *sync.Mutex) adkagent.Agent {
	t.Helper()
	var branches []adkagent.Agent
	for _, name := range []string{"a", "b"} {
		body := branchBody(name, ran, mu)
		run := workflow.NewDynamicNode[any, any]("run_"+name,
			func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (any, error) {
				return workflow.RunNode[any](ctx, body, name, workflow.WithUseAsOutput())
			}, workflow.NodeConfig{})
		branch, err := workflowagent.New(workflowagent.Config{
			Name:        "branch_" + name,
			Description: "branch " + name,
			Edges:       workflow.Chain(workflow.Start, run),
		})
		if err != nil {
			t.Fatalf("workflowagent.New(branch %s): %v", name, err)
		}
		branches = append(branches, branch)
	}
	fan, err := parallelagent.New(parallelagent.Config{AgentConfig: adkagent.Config{
		Name:        "fan",
		Description: "fan",
		SubAgents:   branches,
	}})
	if err != nil {
		t.Fatalf("parallelagent.New: %v", err)
	}
	fanNode, err := workflow.NewAgentNode(fan, workflow.NodeConfig{})
	if err != nil {
		t.Fatalf("NewAgentNode: %v", err)
	}
	dispatch := workflow.NewDynamicNode[any, any]("fan_out",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (any, error) {
			return workflow.RunNode[any](ctx, fanNode, "audit")
		}, workflow.NodeConfig{})
	root, err := workflowagent.New(workflowagent.Config{
		Name:      "agent_probe",
		Edges:     workflow.Chain(workflow.Start, dispatch),
		SubAgents: []adkagent.Agent{fan},
	})
	if err != nil {
		t.Fatalf("workflowagent.New: %v", err)
	}
	return root
}

// runSubstrate runs one probe root to completion and returns everything
// the runner yielded as text, plus the branches that executed.
func runSubstrate(t *testing.T, build func(*testing.T, *[]string, *sync.Mutex) adkagent.Agent) (string, []string) {
	t.Helper()
	var mu sync.Mutex
	var ran []string
	r, err := runner.New(runner.Config{
		AppName:           "graph-test",
		Agent:             build(t, &ran, &mu),
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	var transcript strings.Builder
	for ev, err := range r.Run(context.Background(), "op", "s1",
		genai.NewContentFromText("audit", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil {
				transcript.WriteString(p.Text)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return transcript.String(), append([]string(nil), ran...)
}
