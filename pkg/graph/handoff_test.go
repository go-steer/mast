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
	"bytes"
	"context"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// The diagnoser→executor handoff (v0.4 W7.0). What is pinned here is
// the structural predicate: a finding reaches the change executor when
// it proposed an executable change AND the operator approved it, and
// otherwise the finding is the run's answer. The predicate is the
// graph's, not a model's — no prompt in these tests says "remediate".

// recordingModel wraps a model and keeps every prompt it was sent, so a
// test can ask what a specialist was actually told.
type recordingModel struct {
	model.LLM
	mu      sync.Mutex
	prompts []string
}

func (m *recordingModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	var sb strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				sb.WriteString(p.Text)
				sb.WriteString("\n")
			}
		}
	}
	m.prompts = append(m.prompts, sb.String())
	m.mu.Unlock()
	return m.LLM.GenerateContent(ctx, req, stream)
}

func (m *recordingModel) sawAll() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.prompts, "\n---\n")
}

func (m *recordingModel) ran() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts) > 0
}

// handoffRoster is the smallest roster that can exercise the handoff: a
// classifier, one diagnoser the echo model routes to, a change
// executor, and the required fallback.
type handoffRoster struct {
	root     adkagent.Agent
	executor *recordingModel
}

func newHandoffRoster(t *testing.T, lookup ChangeSetLookup, requireApproval bool) handoffRoster {
	t.Helper()
	exec := &recordingModel{LLM: mastagent.NewEchoModel("echo-change-executor")}
	build := func(name string, m model.LLM) adkagent.Agent {
		a, err := specialists.Build(specialists.Spec{
			Name:        name,
			Description: name + " (test)",
			Mode:        specialists.ModeTask,
			Instruction: "test instruction",
		}, specialists.BuildOptions{Model: m})
		if err != nil {
			t.Fatalf("build specialist %q: %v", name, err)
		}
		return a
	}
	root, err := Build(Config{
		Bundle: workload.Bundle{
			Name:        "w",
			Specialists: []string{"OOMKilled", "change-executor", FallbackName},
			HITL:        workload.HITL{RequireApproval: requireApproval},
		},
		Classifier: buildSpec(t, "clf", specialists.ModeSingleTurn),
		Specialists: map[string]Specialist{
			"OOMKilled": {Agent: build("OOMKilled", mastagent.NewEchoModel("echo-OOMKilled"))},
			"change-executor": {
				Agent:      build("change-executor", exec),
				Capability: specialists.CapabilityChangeExecutor,
			},
			FallbackName: {Agent: build(FallbackName, mastagent.NewEchoModel("echo-fallback"))},
		},
		ApprovedChangeSet: lookup,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return handoffRoster{root: root, executor: exec}
}

// runIncidentEvents drives one incident through the graph and returns
// every event the run produced.
func runIncidentEvents(t *testing.T, root adkagent.Agent) []*session.Event {
	t.Helper()
	r, err := runner.New(runner.Config{
		AppName:           "graph-handoff-test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	incident := `INJECT {"reason":"OOMKilled","namespace":"prod","name":"api"}`
	var events []*session.Event
	for ev, err := range r.Run(context.Background(), "op", "s1",
		genai.NewContentFromText(incident, genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev != nil {
			events = append(events, ev)
		}
	}
	return events
}

// runIncident drives one incident through the graph and returns the
// text of every event the run produced.
func runIncident(t *testing.T, root adkagent.Agent) string {
	t.Helper()
	var sb strings.Builder
	for _, ev := range runIncidentEvents(t, root) {
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text != "" {
				sb.WriteString(p.Text)
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

const approvedCalls = `1. patch_k8s_resource({"name":"api","namespace":"prod"})`

func proposes(t *testing.T, want string) ChangeSetLookup {
	t.Helper()
	return func(_ adkagent.Context, specialist string) (string, bool) {
		if specialist != want {
			return "", false
		}
		return approvedCalls, true
	}
}

// TestApprovalPreambleCarriesTheMarker keeps two strings one string.
// The offline change-executor fake recognizes an approved change set by
// a literal substring of this preamble (agent.ApprovedCallsMarker),
// because pkg/agent cannot import this package — pkg/graph's own tests
// import pkg/agent. Reword the preamble without this test and the fake
// stops executing approved calls silently: every UAT leg still passes,
// having exercised the reason-driven path twice.
func TestApprovalPreambleCarriesTheMarker(t *testing.T) {
	if !strings.Contains(approvalPreamble, mastagent.ApprovedCallsMarker) {
		t.Fatalf("the approval preamble no longer contains %q, so the offline executor fake will not recognize an approved change set:\n%s",
			mastagent.ApprovedCallsMarker, approvalPreamble)
	}
}

// TestApprovedChangeReachesTheExecutor is the handoff working: the
// diagnoser proposed something executable, the workload takes findings
// as approved, and the executor ran — holding the exact calls, not a
// restatement of the incident it would have to re-derive them from.
func TestApprovedChangeReachesTheExecutor(t *testing.T) {
	rst := newHandoffRoster(t, proposes(t, "OOMKilled"), false)
	runIncident(t, rst.root)

	if !rst.executor.ran() {
		t.Fatal("the change executor never ran; an approved, executable finding stopped at the diagnoser")
	}
	if got := rst.executor.sawAll(); !strings.Contains(got, approvedCalls) {
		t.Errorf("the executor was not handed the approved calls, so it would have to re-derive them:\n%s", got)
	}
}

// TestApprovalAnnouncementIsNotUserAuthored pins the one property of
// the announcement event that is invisible in its text and load-bearing
// for the whole feature.
//
// ADK authors an event "user" when its content role is user
// (agent.getAuthorForEvent), and its confirmation resume walks back to
// the most recent user-authored event and gives up if that event holds
// no FunctionResponse
// (llminternal.RequestConfirmationRequestProcessor). The resume pass
// re-emits this announcement, so a user-authored one lands between the
// operator's confirmation and the executor and the approved call is
// never re-dispatched: the run ends idle, nothing applied, no error
// anywhere. Verified end to end by scripts/uat-v0.4.sh U-handoff/A,
// which counts the calls the tool actually received; this test is the
// cheap guard that fails first.
func TestApprovalAnnouncementIsNotUserAuthored(t *testing.T) {
	rst := newHandoffRoster(t, proposes(t, "OOMKilled"), false)

	var found bool
	for _, ev := range runIncidentEvents(t, rst.root) {
		if ev.Content == nil {
			continue
		}
		var text string
		for _, p := range ev.Content.Parts {
			if p != nil {
				text += p.Text
			}
		}
		if !strings.Contains(text, mastagent.ApprovedCallsMarker) {
			continue
		}
		found = true
		if ev.Author == "user" || ev.Content.Role == genai.RoleUser {
			t.Errorf("the approved-calls announcement is user-authored (author %q, role %q); "+
				"on a resume it shadows the operator's confirmation and the approved call never fires",
				ev.Author, ev.Content.Role)
		}
	}
	if !found {
		t.Fatal("no approved-calls announcement on the run's events; the handoff did not happen")
	}
}

// TestNoProposedChangeStopsAtTheFinding is the common case and the one
// that must not regress: most findings propose nothing, and those runs
// have to end exactly as they did before W7.0 — with the finding.
func TestNoProposedChangeStopsAtTheFinding(t *testing.T) {
	nothing := func(adkagent.Context, string) (string, bool) { return "", false }
	rst := newHandoffRoster(t, nothing, false)
	out := runIncident(t, rst.root)

	if rst.executor.ran() {
		t.Error("the change executor ran on a finding that proposed nothing to execute")
	}
	if !strings.Contains(out, "OOMKilled") {
		t.Errorf("the finding did not survive as the run's answer:\n%s", out)
	}
}

// TestUnapprovedChangeStopsAtTheFinding: a proposal is not consent.
// With hitl.require_approval on and no verdict yet, the run parks and
// the executor must not have run — the whole gap the change-safety gate
// exists to hold open.
func TestUnapprovedChangeStopsAtTheFinding(t *testing.T) {
	rst := newHandoffRoster(t, proposes(t, "OOMKilled"), true)
	runIncident(t, rst.root)

	if rst.executor.ran() {
		t.Fatal("the change executor ran before the operator answered; the approval pause did not hold")
	}
}

// TestChangeExecutorIsFoundByCapability: the destination is a declared
// field, not a name convention. A roster that spells its executor
// differently still gets the handoff, and a roster with no executor
// keeps the v0.3 shape.
func TestChangeExecutorIsFoundByCapability(t *testing.T) {
	base := Config{
		Bundle:            workload.Bundle{Name: "w", Specialists: []string{"a", "remediator"}},
		ApprovedChangeSet: func(adkagent.Context, string) (string, bool) { return "", false },
		Specialists: map[string]Specialist{
			"a":          {},
			"remediator": {Capability: specialists.CapabilityChangeExecutor},
		},
	}
	if got := changeExecutor(base); got != "remediator" {
		t.Errorf("executor = %q, want remediator", got)
	}

	noExec := base
	noExec.Specialists = map[string]Specialist{"a": {}}
	if got := changeExecutor(noExec); got != "" {
		t.Errorf("changeExecutor with no executor = %q, want \"\"", got)
	}

	// No lookup means the caller did not ask for the handoff, so the
	// graph keeps its pre-W7.0 shape even for a roster that has an
	// executor in it.
	noLookup := base
	noLookup.ApprovedChangeSet = nil
	if got := changeExecutor(noLookup); got != "" {
		t.Errorf("changeExecutor with no lookup = %q, want \"\"", got)
	}
}

// TestTwoChangeExecutorsDisableTheHandoff: an approved change has to
// have one unambiguous destination, and picking one silently would mean
// the specialist that changes a cluster is decided by map iteration
// order. Refusing the roster outright would be wrong too — a second
// write-capable specialist was legal before the handoff existed and
// still is — so the roster builds with the handoff off, loudly.
func TestTwoChangeExecutorsDisableTheHandoff(t *testing.T) {
	var log bytes.Buffer
	cfg := Config{
		Bundle:     workload.Bundle{Name: "w", Specialists: []string{"e1", "e2", FallbackName}},
		Classifier: buildSpec(t, "clf", specialists.ModeSingleTurn),
		Specialists: map[string]Specialist{
			"e1":         {Agent: buildSpec(t, "e1", specialists.ModeTask), Capability: specialists.CapabilityChangeExecutor},
			"e2":         {Agent: buildSpec(t, "e2", specialists.ModeTask), Capability: specialists.CapabilityChangeExecutor},
			FallbackName: {Agent: buildSpec(t, FallbackName, specialists.ModeTask)},
		},
		ApprovedChangeSet: func(adkagent.Context, string) (string, bool) { return "", false },
		Logger:            slog.New(slog.NewTextHandler(&log, nil)),
	}
	if _, err := Build(cfg); err != nil {
		t.Fatalf("a roster with two change executors no longer builds: %v", err)
	}
	if got := changeExecutor(cfg); got != "" {
		t.Errorf("changeExecutor = %q, want \"\" — an approved change had an arbitrary destination", got)
	}
	// The roster author has to be able to find out why their proposal
	// never executed without reading this package.
	for _, want := range []string{"e1", "e2", "change_executor"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log does not mention %q: %s", want, log.String())
		}
	}
}

// TestVerdictApprovedDefaultsToNo. The two mistakes are not symmetric:
// refusing to execute leaves an operator with a finding; executing on a
// verdict nobody can read leaves them with a changed cluster.
func TestVerdictApprovedDefaultsToNo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict any
		want    bool
	}{
		{"the approval prompt's own shape", map[string]any{"approved": true}, true},
		{"declined", map[string]any{"approved": false}, false},
		{"declined with a note", map[string]any{"approved": false, "note": "not during the freeze"}, false},
		{"a bare bool, as a terser resume payload sends it", true, true},
		{"no approved key at all", map[string]any{"note": "ok"}, false},
		{"approved as a string", map[string]any{"approved": "true"}, false},
		{"a verdict that is just text", "yes please", false},
		{"nothing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verdictApproved(tc.verdict); got != tc.want {
				t.Errorf("verdictApproved(%#v) = %v, want %v", tc.verdict, got, tc.want)
			}
		})
	}
}
