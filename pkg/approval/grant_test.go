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

package approval

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/mast/pkg/permissions"
)

// W7's claim, and what a test has to hold it to: ONE operator answer
// authorizes N calls, and every way that could become "N calls nobody
// approved" is closed.
//
// The seam probe below is deliberately the real thing — real runner,
// real plugin, real SQLite session store, the same three-turn shape a
// change executor produces — because the interesting failures are all
// in the seams: whether a grant written on one event's state delta is
// visible to the next call in the same turn, whether it survives a
// process restart, and whether ADK's re-dispatch renders the same
// signature the operator approved.

// The two calls the fixture specialist proposes. Same tool, different
// object: a change set whose members are individually plausible and
// jointly meaningful, which is the shape the legibility rule is for.
func fixtureChangeSet() []ProposedChange {
	return []ProposedChange{
		{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api", "replicas": float64(3)}},
		{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "worker", "replicas": float64(1)}},
	}
}

func scaleCall(deployment string, replicas int) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", map[string]any{
			"deployment": deployment,
			"replicas":   replicas,
		})},
	}}
}

// readCall records one precondition read mast made on its own behalf.
type readCall struct {
	tool string
	args map[string]any
}

// csProbe is one or more "processes" driving the real write gate over a
// shared session store.
type csProbe struct {
	// executions is one entry per scale_deployment execution.
	executions []scaleArgs
	// reads is every precondition read the gate ran, in order.
	reads []readCall
	// cluster is what a precondition read returns. Mutating it between
	// turns is how a test moves the world under an approval.
	cluster map[string]any
	// readErr, when set, fails the next precondition read.
	readErr error
	// clock is the gate's notion of now, advanced by a test to expire a
	// grant without sleeping.
	clock time.Time

	// gates is one permissions gate per process, so a restart test can
	// assert that nothing but the event log crossed.
	gates []*permissions.Gate
	// confirmations is every parked confirmation call id, in log order,
	// and hints the hint each carried.
	confirmations []string
	hints         []string
	// requests is the typed payload of each parked confirmation.
	requests []Request
	// responses is every scale_deployment tool response, in log order.
	responses []map[string]any
	// state is the durable session state after the last turn.
	state map[string]any
}

type csTurn struct {
	// process indexes the runner this turn runs in. A turn with a
	// higher index than the previous one is a restart: a new agent, new
	// plugin, new permissions gate, new handle on the store.
	process int
	// send builds the turn's input. Verdicts read the probe's last
	// parked confirmation id.
	send func(p *csProbe) *genai.Content
}

type csConfig struct {
	// set is seeded into session state as a specialist's recorded
	// change set. Nil records nothing, which is how a test asserts that
	// a call outside any set cannot be granted.
	set        []ProposedChange
	specialist string

	// noGrants builds the gate without a Freshness, i.e. W7.0.
	noGrants bool
	// ttl overrides DefaultGrantTTL.
	ttl time.Duration
	// preconditions declares a freshness read per tool.
	preconditions map[string]*Precondition
	// preconditionErr, when set, is what the declaration lookup returns.
	preconditionErr error

	gateOptions permissions.Options
	// scripts is the model script per process.
	scripts [][]*model.LLMResponse
	turns   []csTurn
	// cluster is the precondition read's initial answer.
	cluster map[string]any
}

func runChangeSetProbe(t *testing.T, cfg csConfig) *csProbe {
	t.Helper()
	probe := &csProbe{
		cluster: cfg.cluster,
		clock:   time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC),
	}
	if probe.cluster == nil {
		probe.cluster = map[string]any{"replicas": float64(3)}
	}
	dir := t.TempDir()

	build := func(process int) (*runner.Runner, adksession.Service) {
		t.Helper()
		scale, err := functiontool.New(functiontool.Config{
			Name:        "scale_deployment",
			Description: "changes a deployment's replica count",
		}, func(_ adkagent.Context, args scaleArgs) (map[string]any, error) {
			probe.executions = append(probe.executions, args)
			return map[string]any{"scaled": args.Replicas}, nil
		})
		if err != nil {
			t.Fatalf("functiontool.New: %v", err)
		}

		script := cfg.scripts[0]
		if process < len(cfg.scripts) {
			script = cfg.scripts[process]
		}
		root, err := llmagent.New(llmagent.Config{
			Name:        "change_executor",
			Description: "change-set probe",
			Instruction: "act",
			Model:       &scriptedModel{name: "grants", calls: script},
			Tools:       []tool.Tool{scale},
		})
		if err != nil {
			t.Fatalf("llmagent.New: %v", err)
		}

		var grants *Freshness
		if !cfg.noGrants {
			grants = &Freshness{
				TTL: cfg.ttl,
				Now: func() time.Time { return probe.clock },
				Precondition: func(name string) (*Precondition, error) {
					if cfg.preconditionErr != nil {
						return nil, cfg.preconditionErr
					}
					return cfg.preconditions[name], nil
				},
				Read: func(_ adkagent.Context, name string, args map[string]any) (map[string]any, error) {
					probe.reads = append(probe.reads, readCall{tool: name, args: args})
					if probe.readErr != nil {
						return nil, probe.readErr
					}
					out := map[string]any{}
					for k, v := range probe.cluster {
						out[k] = v
					}
					return out, nil
				},
			}
		}

		gate := permissions.New(cfg.gateOptions)
		probe.gates = append(probe.gates, gate)
		wg, err := New(Config{
			Policy:   OnMutationRequireApproval,
			Mutating: alwaysMutating,
			Gate:     gate,
			Grants:   grants,
		})
		if err != nil {
			t.Fatalf("approval.New: %v", err)
		}

		svc := sqliteServiceAt(t, dir)
		r, err := runner.New(runner.Config{
			AppName:           testApp,
			Agent:             root,
			SessionService:    svc,
			AutoCreateSession: true,
			PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{wg}},
		})
		if err != nil {
			t.Fatalf("runner.New: %v", err)
		}
		return r, svc
	}

	current := -1
	var r *runner.Runner
	var svc adksession.Service
	for i, turn := range cfg.turns {
		if turn.process != current {
			r, svc = build(turn.process)
			current = turn.process
			if i == 0 && cfg.set != nil {
				seedChangeSet(t, svc, cfg.specialist, cfg.set)
			}
		}
		msg := turn.send(probe)
		if msg == nil {
			t.Fatalf("turn %d produced no input (no confirmation to answer?)", i)
		}
		for _, err := range r.Run(context.Background(), testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("turn %d: runner.Run: %v", i, err)
			}
		}
		probe.confirmations, probe.hints, probe.requests = confirmationRequests(t, svc)
	}
	probe.responses = toolResponses(t, svc, "scale_deployment")
	probe.state = sessionState(t, svc)
	return probe
}

// seedChangeSet writes a specialist's recorded change set into session
// state, standing in for the finish_task pass that recorded it (W7.0).
func seedChangeSet(t *testing.T, svc adksession.Service, specialist string, set []ProposedChange) {
	t.Helper()
	if specialist == "" {
		specialist = "diagnoser"
	}
	raw, err := EncodeChangeSet(set)
	if err != nil {
		t.Fatalf("EncodeChangeSet: %v", err)
	}
	got, err := svc.Create(context.Background(), &adksession.CreateRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ev := &adksession.Event{
		InvocationID: "seed-invocation",
		Author:       specialist,
		Timestamp:    time.Now().Add(-time.Minute),
		Actions:      adksession.EventActions{StateDelta: map[string]any{ChangeSetStateKey(specialist): raw}},
	}
	if err := svc.AppendEvent(context.Background(), got.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

// confirmationRequests collects every parked confirmation in log order,
// with the hint and the typed payload each carried. The plural matters:
// the whole question W7 answers is how many times an operator is asked.
func confirmationRequests(t *testing.T, svc adksession.Service) (ids, hints []string, reqs []Request) {
	t.Helper()
	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for ev := range got.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part == nil || part.FunctionCall == nil || part.FunctionCall.Name != toolconfirmation.FunctionCallName {
				continue
			}
			p := DescribeConfirmation(part.FunctionCall.Args)
			ids = append(ids, part.FunctionCall.ID)
			hints = append(hints, p.Hint)
			req, err := DecodeRequest(p.Request)
			if err != nil {
				t.Fatalf("parked payload is not an approval request: %v", err)
			}
			reqs = append(reqs, req)
		}
	}
	return ids, hints, reqs
}

// answerLast builds the operator's turn against the most recent parked
// confirmation.
func answerLast(payload map[string]any) func(*csProbe) *genai.Content {
	return func(p *csProbe) *genai.Content {
		if len(p.confirmations) == 0 {
			return nil
		}
		return verdictResponse(p.confirmations[len(p.confirmations)-1], map[string]any{
			"confirmed": true, "payload": payload,
		})
	}
}

// modelSays is a script entry that ends the turn talking instead of
// calling another tool.
func modelSays(text string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel)}
}

func userSays(text string) func(*csProbe) *genai.Content {
	return func(*csProbe) *genai.Content { return genai.NewContentFromText(text, genai.RoleUser) }
}

// approveSet is the verdict under test: yes to this call, and to the
// rest of the set it belongs to.
func approveSet() map[string]any {
	return map[string]any{"verdict": "approve", "scope": "change_set", "approver": "user:sre-oncall"}
}

func (p *csProbe) approvals() []permissions.ApprovalLog {
	var out []permissions.ApprovalLog
	for _, g := range p.gates {
		out = append(out, g.Approvals()...)
	}
	return out
}

// grants reads every grant record out of the durable session state.
func (p *csProbe) grants(t *testing.T) []Grant {
	t.Helper()
	var out []Grant
	for k, v := range p.state {
		if !strings.HasPrefix(k, GrantStateKeyPrefix) {
			continue
		}
		g, err := DecodeGrant(v)
		if err != nil {
			t.Fatalf("state[%q] is not a grant record: %v", k, err)
		}
		out = append(out, g)
	}
	return out
}

func (p *csProbe) executed(deployment string, replicas int) bool {
	for _, e := range p.executions {
		if e.Deployment == deployment && e.Replicas == replicas {
			return true
		}
	}
	return false
}

// twoCallTurns is the shape a change executor actually produces: park
// on the first call, answer it, and the second call follows in the same
// turn.
//
// The script is two entries, not three, because a park ENDS the turn —
// the "awaiting approval" response is written for the log and the model
// is not called again until the operator answers (pinned above by the
// round counts in the seam probes). So index 0 is the parked call and
// index 1 is what the model says once the approved call has run.
func twoCallTurns(payload map[string]any, second *model.LLMResponse) ([][]*model.LLMResponse, []csTurn) {
	scripts := [][]*model.LLMResponse{{scaleCall("api", 3), second}}
	return scripts, []csTurn{
		{send: userSays("remediate the api outage")},
		{send: answerLast(payload)},
	}
}

// TestChangeSetApprovalAuthorizesTheRestOfTheSet is W7's core claim: one
// answer, N calls, no second question.
func TestChangeSetApprovalAuthorizesTheRestOfTheSet(t *testing.T) {
	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 1))
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), scripts: scripts, turns: turns})

	if len(p.confirmations) != 1 {
		t.Fatalf("operator was asked %d time(s), want exactly 1 — the second call should have been authorized by the change-set approval\nhints: %q", len(p.confirmations), p.hints)
	}
	if len(p.executions) != 2 {
		t.Fatalf("tool executed %d time(s), want 2: %+v", len(p.executions), p.executions)
	}
	if !p.executed("api", 3) || !p.executed("worker", 1) {
		t.Errorf("executions = %+v, want both calls of the approved set", p.executions)
	}

	// The grant is spent, not deleted: the record is the audit answer to
	// "who authorized the call nobody was asked about".
	grants := p.grants(t)
	if len(grants) != 1 {
		t.Fatalf("grant records = %+v, want exactly one (the set's other call)", grants)
	}
	g := grants[0]
	if g.ConsumedBy == "" {
		t.Errorf("grant %+v was never marked consumed", g)
	}
	if g.Approver != "user:sre-oncall" {
		t.Errorf("grant.Approver = %q, want the operator who answered", g.Approver)
	}
	if want := `scale_deployment({"deployment":"worker","replicas":1})`; g.Signature != want {
		t.Errorf("grant.Signature = %q, want %q", g.Signature, want)
	}
	if !strings.Contains(g.Origin, "deployment=api") {
		t.Errorf("grant.Origin = %q, want the call the operator actually answered", g.Origin)
	}

	// Both calls are in the approval log. The grant removes the
	// question, never the accounting.
	log := p.approvals()
	if len(log) != 2 {
		t.Fatalf("approval log = %+v, want one entry per executed call", log)
	}
	for _, e := range log {
		if e.Decision != permissions.DecisionAllowOnce {
			t.Errorf("approval %+v is not an allow-once", e)
		}
	}
}

// TestGrantAuthorizesOnlyTheExactCall: the grant is bound to the bytes
// of the call, not to the tool. A model that proposes replicas=1 and
// then calls replicas=20 is asking a question nobody has answered.
func TestGrantAuthorizesOnlyTheExactCall(t *testing.T) {
	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 20))
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), scripts: scripts, turns: turns})

	if len(p.executions) != 1 || !p.executed("api", 3) {
		t.Fatalf("executions = %+v, want only the call the operator answered directly", p.executions)
	}
	if len(p.confirmations) != 2 {
		t.Fatalf("operator was asked %d time(s), want 2 — a call outside the approved set must park", len(p.confirmations))
	}
	if grants := p.grants(t); len(grants) != 1 || grants[0].ConsumedBy != "" {
		t.Errorf("grant records = %+v, want the worker/1 grant still unspent", grants)
	}
}

// TestGrantExpires: the wall-clock backstop. An approval answered from a
// phone at 02:00 and executed when the daemon comes back at 09:00 is not
// an approval of anything anyone looked at.
func TestGrantExpires(t *testing.T) {
	scripts := [][]*model.LLMResponse{
		{scaleCall("api", 3), modelSays("the first change is done")},
		{scaleCall("worker", 1)},
	}
	turns := []csTurn{
		{process: 0, send: userSays("remediate")},
		{process: 0, send: answerLast(approveSet())},
		{process: 1, send: func(p *csProbe) *genai.Content {
			p.clock = p.clock.Add(2 * time.Hour)
			return genai.NewContentFromText("carry on", genai.RoleUser)
		}},
	}
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), ttl: 10 * time.Minute, scripts: scripts, turns: turns})

	if p.executed("worker", 1) {
		t.Fatalf("an expired grant ran the call: %+v", p.executions)
	}
	if len(p.confirmations) != 2 {
		t.Fatalf("operator was asked %d time(s), want 2 — an expired approval must re-park", len(p.confirmations))
	}
	// The operator is told their own earlier answer stopped covering
	// this, rather than being asked the same question with no context.
	last := p.requests[len(p.requests)-1]
	if !strings.Contains(last.Stale, "expired") {
		t.Errorf("re-parked request.Stale = %q, want it to say the approval expired", last.Stale)
	}
	if !strings.Contains(p.hints[len(p.hints)-1], "asking again") {
		t.Errorf("re-parked hint = %q, want it to say mast is asking again", p.hints[len(p.hints)-1])
	}
	grants := p.grants(t)
	if len(grants) != 1 || grants[0].VoidedBy == "" {
		t.Fatalf("grant records = %+v, want the expired grant voided", grants)
	}
}

// TestGrantSurvivesARestart: the grant is durable, because the calls it
// authorizes happen after a resume and possibly after a crash, in a
// process that does not remember the first pass.
func TestGrantSurvivesARestart(t *testing.T) {
	scripts := [][]*model.LLMResponse{
		{scaleCall("api", 3), modelSays("the first change is done")},
		{scaleCall("worker", 1)},
	}
	turns := []csTurn{
		{process: 0, send: userSays("remediate")},
		{process: 0, send: answerLast(approveSet())},
		{process: 1, send: userSays("carry on")},
	}
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), scripts: scripts, turns: turns})

	if !p.executed("worker", 1) {
		t.Fatalf("the granted call did not run after a restart: %+v (parks: %q)", p.executions, p.hints)
	}
	if len(p.confirmations) != 1 {
		t.Fatalf("operator was asked %d time(s) across the restart, want 1", len(p.confirmations))
	}
	// Nothing but the event log crossed: the second process's gate holds
	// only the approval it granted itself.
	if got := p.gates[1].Approvals(); len(got) != 1 || !strings.Contains(got[0].Key, "worker") {
		t.Errorf("post-restart approval log = %+v, want only the granted call", got)
	}
}

// TestGrantIsSingleUse: a grant authorizes one call, once. An executor
// that repeats an approved call is asking for a second mutation, and a
// second mutation is a second question.
func TestGrantIsSingleUse(t *testing.T) {
	scripts := [][]*model.LLMResponse{
		{scaleCall("api", 3), modelSays("the first change is done")},
		{scaleCall("worker", 1), scaleCall("worker", 1)},
	}
	turns := []csTurn{
		{process: 0, send: userSays("remediate")},
		{process: 0, send: answerLast(approveSet())},
		{process: 1, send: userSays("carry on")},
	}
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), scripts: scripts, turns: turns})

	worker := 0
	for _, e := range p.executions {
		if e.Deployment == "worker" {
			worker++
		}
	}
	if worker != 1 {
		t.Fatalf("the granted call ran %d time(s), want exactly 1: %+v", worker, p.executions)
	}
	if len(p.confirmations) != 2 {
		t.Fatalf("operator was asked %d time(s), want 2 — the repeat must park", len(p.confirmations))
	}
	if last := p.requests[len(p.requests)-1]; !strings.Contains(last.Stale, "already been made") {
		t.Errorf("re-parked request.Stale = %q, want it to say the call was already made once", last.Stale)
	}
}

// TestGrantIsVoidedWhenTheClusterMoves is the half a wall clock cannot
// do. A change set is self-invalidating by construction — call 1 mutates
// the world call 2 was reasoned about — so the declared precondition is
// re-read before the granted call fires.
func TestGrantIsVoidedWhenTheClusterMoves(t *testing.T) {
	pre := map[string]*Precondition{"scale_deployment": {
		Read:     "get_deployment",
		ArgsFrom: map[string]string{"name": "deployment"},
		Fields:   []string{"replicas"},
	}}
	scripts := [][]*model.LLMResponse{
		{scaleCall("api", 3), modelSays("the first change is done")},
		{scaleCall("worker", 1)},
	}
	turns := []csTurn{
		{process: 0, send: userSays("remediate")},
		{process: 0, send: answerLast(approveSet())},
		{process: 1, send: func(p *csProbe) *genai.Content {
			// Somebody else scaled it while the operator was reading.
			p.cluster = map[string]any{"replicas": float64(9)}
			return genai.NewContentFromText("carry on", genai.RoleUser)
		}},
	}
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), preconditions: pre, scripts: scripts, turns: turns})

	if p.executed("worker", 1) {
		t.Fatalf("a granted call ran against a cluster that had moved: %+v", p.executions)
	}
	if len(p.confirmations) != 2 {
		t.Fatalf("operator was asked %d time(s), want 2", len(p.confirmations))
	}
	last := p.requests[len(p.requests)-1]
	if !strings.Contains(last.Stale, "replicas") || !strings.Contains(last.Stale, "9") {
		t.Errorf("request.Stale = %q, want it to name the field that moved and its new value — \"something changed\" is not an answer an operator can act on", last.Stale)
	}
	// The read is mast's own and takes the change's own argument.
	if len(p.reads) < 2 {
		t.Fatalf("precondition reads = %+v, want one at mint and one before the call fires", p.reads)
	}
	for _, r := range p.reads {
		if r.tool != "get_deployment" || r.args["name"] != "worker" {
			t.Errorf("precondition read %+v, want get_deployment(name=worker) — args_from maps the change's own arguments", r)
		}
	}
	if grants := p.grants(t); len(grants) != 1 || grants[0].VoidedBy == "" {
		t.Errorf("grant records = %+v, want the grant voided, not merely skipped", grants)
	}
}

// TestUnchangedPreconditionStillFires: the other half of the same
// mechanism. A precondition that holds must not cost the operator a
// question — a freshness check that fires on a still cluster would train
// exactly the rubber-stamping it exists to prevent.
func TestUnchangedPreconditionStillFires(t *testing.T) {
	pre := map[string]*Precondition{"scale_deployment": {
		Read:     "get_deployment",
		ArgsFrom: map[string]string{"name": "deployment"},
		Fields:   []string{"replicas"},
	}}
	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 1))
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), preconditions: pre, scripts: scripts, turns: turns})

	if !p.executed("worker", 1) {
		t.Fatalf("the granted call did not run against an unchanged cluster: %+v (parks: %q)", p.executions, p.hints)
	}
	if len(p.confirmations) != 1 {
		t.Fatalf("operator was asked %d time(s), want 1", len(p.confirmations))
	}
}

// TestChangeSetVerdictRefusals covers every reason mast declines to turn
// one answer into N, and asserts the same three things each time: the
// call in hand did NOT run, no grant was written, and the model is told
// to go back to one-at-a-time.
func TestChangeSetVerdictRefusals(t *testing.T) {
	tests := []struct {
		name    string
		cfg     csConfig
		payload map[string]any
		wants   []string
	}{
		{
			name:    "no change set was recorded",
			cfg:     csConfig{},
			payload: approveSet(),
			wants:   []string{"not one of the calls in any change set"},
		},
		{
			name:    "this deployment does not issue change-set approvals",
			cfg:     csConfig{set: fixtureChangeSet(), noGrants: true},
			payload: approveSet(),
			wants:   []string{"does not issue change-set approvals"},
		},
		{
			name: "the verdict edits the call",
			cfg:  csConfig{set: fixtureChangeSet()},
			payload: map[string]any{
				"verdict": "edit", "scope": "change_set", "approver": "user:sre-oncall",
				"args": map[string]any{"deployment": "api", "replicas": 2},
			},
			wants: []string{"speaks only for the call it edits"},
		},
		{
			name: "a call in the set is too large to read",
			cfg: csConfig{set: []ProposedChange{
				{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api", "replicas": float64(3)}},
				{Tool: "scale_deployment", Arguments: map[string]any{"deployment": strings.Repeat("x", 200), "replicas": float64(1)}},
			}},
			payload: approveSet(),
			wants:   []string{"too large to show in the approval question"},
		},
		{
			name: "a precondition declaration cannot be read",
			cfg: csConfig{
				set:             fixtureChangeSet(),
				preconditionErr: fmt.Errorf("the bundle names a read tool this daemon does not hold"),
			},
			payload: approveSet(),
			wants:   []string{"could not record what", "assumes about the cluster"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			// The model reports the refusal rather than calling again: a
			// second call would park on its own and its "awaiting" response
			// would be the one these assertions read.
			cfg.scripts, cfg.turns = twoCallTurns(tt.payload, modelSays("mast refused the change set"))
			p := runChangeSetProbe(t, cfg)

			if len(p.executions) != 0 {
				t.Fatalf("tool executed %d time(s) on a refused change-set verdict, want 0: %+v", len(p.executions), p.executions)
			}
			if grants := p.grants(t); len(grants) != 0 {
				t.Fatalf("grant records = %+v, want none — a refused verdict must leave no authorization behind", grants)
			}
			resp := p.responses[len(p.responses)-1]
			wantField(t, resp, "error", "approval_scope_refused")
			wantDetailMentions(t, resp, append(tt.wants, "one at a time")...)
		})
	}
}

// TestPreconditionReadFailureAtMintRefusesTheSet: the read is declared
// and runnable, and it fails. Same rule as a missing declaration — mast
// will not mint an approval it cannot later check.
func TestPreconditionReadFailureAtMintRefusesTheSet(t *testing.T) {
	pre := map[string]*Precondition{"scale_deployment": {Read: "get_deployment"}}
	scripts, turns := twoCallTurns(approveSet(), modelSays("mast refused the change set"))
	turns[1] = csTurn{send: func(p *csProbe) *genai.Content {
		p.readErr = fmt.Errorf("connection refused")
		return answerLast(approveSet())(p)
	}}
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), preconditions: pre, scripts: scripts, turns: turns})

	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) when the precondition could not be snapshotted, want 0: %+v", len(p.executions), p.executions)
	}
	if grants := p.grants(t); len(grants) != 0 {
		t.Fatalf("grant records = %+v, want none", grants)
	}
	resp := p.responses[len(p.responses)-1]
	wantField(t, resp, "error", "approval_scope_refused")
	wantDetailMentions(t, resp, "connection refused", "one at a time")
}

// TestGrantDoesNotEscapeADenyPolicy is why CheckMutatingToolCall runs in
// FRONT of the grant check. A configured deny is not something an
// operator's approval overrides — it is the rule they wrote precisely so
// they would not be asked.
func TestGrantDoesNotEscapeADenyPolicy(t *testing.T) {
	policy, err := permissions.NewPolicy(nil, []string{"scale_deployment:*deployment=worker*"})
	if err != nil {
		t.Fatal(err)
	}
	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 1))
	p := runChangeSetProbe(t, csConfig{
		set:         fixtureChangeSet(),
		gateOptions: permissions.Options{Mode: permissions.ModeYolo, Policy: policy},
		scripts:     scripts,
		turns:       turns,
	})

	if p.executed("worker", 1) {
		t.Fatalf("a granted call ran into denied territory: %+v", p.executions)
	}
	if !p.executed("api", 3) {
		t.Fatalf("the approved call did not run: %+v", p.executions)
	}
	last := p.responses[len(p.responses)-1]
	wantField(t, last, "error", "denied_by_policy")
	// The grant is still on record and still unspent — the deny is about
	// this call, not about the approval.
	if grants := p.grants(t); len(grants) != 1 || grants[0].ConsumedBy != "" {
		t.Errorf("grant records = %+v, want the grant unspent", grants)
	}
}

// TestParkedQuestionCarriesTheSet: an operator answering `change_set` is
// authorizing calls other than the one in the question, so the question
// has to show them. Both in the payload a client renders and in the hint
// everything renders always.
func TestParkedQuestionCarriesTheSet(t *testing.T) {
	pre := map[string]*Precondition{"scale_deployment": {Read: "get_deployment"}}
	scripts := [][]*model.LLMResponse{{scaleCall("api", 3)}}
	turns := []csTurn{{send: userSays("remediate")}}
	p := runChangeSetProbe(t, csConfig{
		set: fixtureChangeSet(), preconditions: pre, ttl: 15 * time.Minute,
		scripts: scripts, turns: turns,
	})

	if len(p.requests) != 1 {
		t.Fatalf("parked requests = %d, want 1", len(p.requests))
	}
	set := p.requests[0].ChangeSet
	if set == nil {
		t.Fatal("the parked question carries no change set, so an operator cannot answer scope=change_set honestly")
	}
	if set.Specialist != "diagnoser" || len(set.Changes) != 2 {
		t.Errorf("change set = %+v, want the diagnoser's two calls", set)
	}
	if !set.Grantable || set.Ungrantable != "" {
		t.Errorf("change set = %+v, want it grantable", set)
	}
	if set.TTLSeconds != 900 {
		t.Errorf("TTLSeconds = %d, want 900", set.TTLSeconds)
	}
	if got := set.Preconditions["scale_deployment"]; !strings.Contains(got, "get_deployment") {
		t.Errorf("precondition description = %q, want it to name the read", got)
	}
	if !strings.Contains(p.hints[0], "scope=change_set") || !strings.Contains(p.hints[0], "2") {
		t.Errorf("hint = %q, want it to offer the whole set", p.hints[0])
	}
}

// TestUngrantableSetSaysSoInTheQuestion: a set mast will not grant must
// not advertise `scope: change_set`, and must say why — an operator who
// answers change_set and gets a refusal has been sent round a loop for
// nothing.
func TestUngrantableSetSaysSoInTheQuestion(t *testing.T) {
	big := []ProposedChange{
		{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api", "replicas": float64(3)}},
		{Tool: "scale_deployment", Arguments: map[string]any{"deployment": strings.Repeat("x", 200), "replicas": float64(1)}},
	}
	scripts := [][]*model.LLMResponse{{scaleCall("api", 3)}}
	p := runChangeSetProbe(t, csConfig{set: big, scripts: scripts, turns: []csTurn{{send: userSays("remediate")}}})

	set := p.requests[0].ChangeSet
	if set == nil {
		t.Fatal("no change set in the parked question")
	}
	if set.Grantable {
		t.Errorf("change set = %+v, want it marked ungrantable", set)
	}
	if !strings.Contains(set.Ungrantable, "too large") {
		t.Errorf("Ungrantable = %q, want the reason", set.Ungrantable)
	}
	if !strings.Contains(p.hints[0], "one at a time") {
		t.Errorf("hint = %q, want it to say the calls must be approved one at a time", p.hints[0])
	}
}

// TestNoChangeSetLeavesTheQuestionUnchanged: the gate is invisible to a
// call nobody proposed as part of a set. W7 adds a path, it does not
// change the one W2 shipped.
func TestNoChangeSetLeavesTheQuestionUnchanged(t *testing.T) {
	scripts := [][]*model.LLMResponse{{scaleCall("api", 3)}}
	p := runChangeSetProbe(t, csConfig{scripts: scripts, turns: []csTurn{{send: userSays("remediate")}}})

	if p.requests[0].ChangeSet != nil {
		t.Errorf("request.ChangeSet = %+v, want nil for a call outside any recorded set", p.requests[0].ChangeSet)
	}
	if want := "Approve mutating call scale_deployment(deployment=api, replicas=3)?"; p.hints[0] != want {
		t.Errorf("hint = %q, want %q", p.hints[0], want)
	}
}

// --- unit tests for the pieces the seam exercises end to end ---

func TestLegible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changes []ProposedChange
		wantErr string
	}{
		{
			name:    "ordinary named arguments",
			changes: fixtureChangeSet(),
		},
		{
			name: "a long string argument",
			changes: []ProposedChange{
				{Tool: "apply_manifest", Arguments: map[string]any{"manifest": strings.Repeat("y", 200)}},
			},
			wantErr: `calls apply_manifest with a "manifest" argument too large`,
		},
		{
			name: "a nested object that renders long",
			changes: []ProposedChange{
				{Tool: "patch", Arguments: map[string]any{"patch": map[string]any{"spec": strings.Repeat("z", 200)}}},
			},
			wantErr: "too large",
		},
		{
			name: "exactly at the limit is still legible",
			changes: []ProposedChange{
				{Tool: "apply_manifest", Arguments: map[string]any{"manifest": strings.Repeat("y", maxValueLen)}},
			},
		},
		{
			name:    "the second entry is the offender",
			changes: []ProposedChange{{Tool: "a", Arguments: map[string]any{"x": 1}}, {Tool: "b", Arguments: map[string]any{"y": strings.Repeat("q", 300)}}},
			wantErr: "[1] calls b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Legible(tt.changes)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Legible = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Legible = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestDigestResultIsStableAcrossMapOrder(t *testing.T) {
	t.Parallel()
	result := map[string]any{"a": 1, "b": 2, "c": map[string]any{"d": 3, "e": 4}, "f": 5, "g": 6, "h": 7}
	first, _, err := digestResult(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		got, _, err := digestResult(result, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("digest render %d = %q, want %q — a digest that depends on map iteration order would report drift on every check", i, got, first)
		}
	}
}

func TestDigestResultFields(t *testing.T) {
	t.Parallel()
	result := map[string]any{"spec": map[string]any{"replicas": float64(3)}, "status": "Running"}

	_, fields, err := digestResult(result, []string{"spec.replicas", "status"})
	if err != nil {
		t.Fatalf("digestResult: %v", err)
	}
	if fields["spec.replicas"] != "3" || fields["status"] != `"Running"` {
		t.Errorf("fields = %v, want the rendered values at the declared paths", fields)
	}

	// Fail closed: a declared field the read does not return means the
	// declaration and the tool disagree, and comparing nothing to
	// nothing always passes.
	if _, _, err := digestResult(result, []string{"spec.image"}); err == nil {
		t.Error("digestResult accepted a declared field the result does not carry")
	}
	if _, _, err := digestResult(result, []string{"status.phase"}); err == nil {
		t.Error("digestResult walked into a non-object and called it a match")
	}
}

func TestPreconditionReadArgs(t *testing.T) {
	t.Parallel()
	ch := ProposedChange{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api", "namespace": "prod", "replicas": float64(5)}}

	p := Precondition{
		Read:     "get_deployment",
		Args:     map[string]any{"output": "json"},
		ArgsFrom: map[string]string{"name": "deployment", "namespace": "namespace"},
	}
	args, err := p.readArgs(ch)
	if err != nil {
		t.Fatalf("readArgs: %v", err)
	}
	if args["name"] != "api" || args["namespace"] != "prod" || args["output"] != "json" {
		t.Errorf("readArgs = %v, want the change's own arguments mapped onto the read's", args)
	}

	// A named argument the change does not carry means the declaration
	// does not describe this call; guessing would check the wrong object.
	missing := Precondition{Read: "get_deployment", ArgsFrom: map[string]string{"name": "workload"}}
	if _, err := missing.readArgs(ch); err == nil {
		t.Error("readArgs accepted a declaration naming an argument the call does not carry")
	}

	if _, err := (Precondition{}).readArgs(ch); err == nil {
		t.Error("readArgs accepted a precondition with no read tool")
	}
}

func TestGrantSpent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		grant Grant
		id    string
		want  bool
	}{
		{"live", Grant{}, "call-1", false},
		{"voided", Grant{VoidedBy: "the cluster moved"}, "call-1", true},
		{"consumed by another call", Grant{ConsumedBy: "call-0"}, "call-1", true},
		// ADK re-dispatches a call after a confirmation, so a grant spent
		// by THIS call is not spent as far as this call is concerned.
		{"consumed by this call", Grant{ConsumedBy: "call-1"}, "call-1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.grant.Spent(tt.id); got != tt.want {
				t.Errorf("Spent(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestGrantRoundTrip(t *testing.T) {
	t.Parallel()
	want := Grant{
		Signature: `scale_deployment({"deployment":"api","replicas":3})`,
		Tool:      "scale_deployment",
		Arguments: map[string]any{"deployment": "api", "replicas": float64(3)},
		Origin:    "scale_deployment(deployment=worker, replicas=1)",
		Approver:  "user:sre-oncall",
		MintedAt:  time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 8, 15, 9, 10, 0, 0, time.UTC),
		Precondition: &PreconditionSnapshot{
			Read: "get_deployment", Args: map[string]any{"name": "api"},
			Digest: "abc123", Fields: map[string]string{"replicas": "3"},
		},
	}
	raw, err := EncodeGrant(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeGrant(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Signature != want.Signature || got.Approver != want.Approver ||
		!got.ExpiresAt.Equal(want.ExpiresAt) || got.Precondition == nil ||
		got.Precondition.Digest != "abc123" || got.Precondition.Fields["replicas"] != "3" {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if _, err := DecodeGrant(nil); err == nil {
		t.Error("DecodeGrant(nil) accepted an absent record")
	}
	if _, err := DecodeGrant("not json"); err == nil {
		t.Error("DecodeGrant accepted a value that is not a grant")
	}
}

func TestGrantStateKeyIsPerSignature(t *testing.T) {
	t.Parallel()
	a, _ := Signature("scale_deployment", map[string]any{"deployment": "api", "replicas": 3})
	b, _ := Signature("scale_deployment", map[string]any{"deployment": "api", "replicas": 4})
	if GrantStateKey(a) == GrantStateKey(b) {
		t.Fatal("two different calls share a grant key, so one call's consumption would erase the other's")
	}
	if !strings.HasPrefix(GrantStateKey(a), GrantStateKeyPrefix) {
		t.Errorf("GrantStateKey = %q, want the %q namespace", GrantStateKey(a), GrantStateKeyPrefix)
	}
	// Determinism is what makes the key usable as a lookup: the same
	// call written with its arguments in another order has to land on
	// the same record, or a grant would be minted under one key and
	// looked for under another.
	reordered, _ := Signature("scale_deployment", map[string]any{"replicas": 3, "deployment": "api"})
	if GrantStateKey(reordered) != GrantStateKey(a) {
		t.Error("the same call with its arguments in another order gets a different grant key")
	}
}

func TestDefaultGrantTTLApplies(t *testing.T) {
	t.Parallel()
	var f *Freshness
	if got := f.ttl(); got != DefaultGrantTTL {
		t.Errorf("nil Freshness ttl = %v, want %v", got, DefaultGrantTTL)
	}
	if got := (&Freshness{}).ttl(); got != DefaultGrantTTL {
		t.Errorf("zero TTL = %v, want %v", got, DefaultGrantTTL)
	}
	if got := (&Freshness{TTL: time.Minute}).ttl(); got != time.Minute {
		t.Errorf("TTL = %v, want a minute", got)
	}
}

// TestVerifyWithoutAReadDoesNotSayNothingChanged: a deployment that can
// no longer evaluate a precondition it once could — the bundle changed,
// the read tool went away — must re-park, not assume the world held
// still.
func TestVerifyWithoutAReadDoesNotSayNothingChanged(t *testing.T) {
	t.Parallel()
	snap := &PreconditionSnapshot{Read: "get_deployment", Digest: "abc"}
	if got := (&Freshness{}).verify(nil, snap, "scale_deployment"); got == "" {
		t.Error("verify returned \"nothing changed\" with no way to read the cluster")
	}
	// No snapshot means no declared precondition, which is the TTL-only
	// case and is not a failure.
	if got := (&Freshness{}).verify(nil, nil, "scale_deployment"); got != "" {
		t.Errorf("verify with no precondition = %q, want it to pass", got)
	}
}

func TestScopeChangeSetIsAdmissible(t *testing.T) {
	t.Parallel()
	d, err := Verdict{Verdict: OutcomeApprove, Scope: ScopeChangeSet}.Decision()
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	// Allow-once, not a broader permissions grant: the reach of a change
	// set lives in mast's own grant records, which expire and are
	// re-checked. permissions.Gate never learns a pattern.
	if d != permissions.DecisionAllowOnce {
		t.Errorf("Decision = %v, want allow-once", d)
	}
	enum := VerdictSchema().Properties["scope"].Enum
	found := false
	for _, v := range enum {
		if v == string(ScopeChangeSet) {
			found = true
		}
	}
	if !found {
		t.Errorf("scope enum = %v, want it to offer %q", enum, ScopeChangeSet)
	}
}
