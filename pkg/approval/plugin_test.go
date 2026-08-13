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

	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/permissions"
)

// alwaysMutating is the predicate the gate is built with in most of
// these tests: the tool under probe is the mutating one.
func alwaysMutating(string) bool { return true }

// gateProbe is one end-to-end run of the real plugin under a real
// runner and a real SQLite session store.
type gateProbe struct {
	// executions is one entry per actual tool execution.
	executions []scaleArgs
	// responses is the tool response the flow recorded for each
	// scale_deployment call, in order — what the model was told.
	responses []map[string]any
	// confirmationID is the parked confirmation, empty if none.
	confirmationID string
	hintSeen       string
	// gate is the permissions gate the plugin consulted, so a test can
	// read its approval log.
	gate *permissions.Gate
}

type gateProbeConfig struct {
	// policy is the workload's on_mutation policy.
	policy OnMutation
	// mutating overrides the predicate; nil means alwaysMutating.
	mutating func(string) bool
	// gateOptions builds the permissions gate; zero value means an
	// ordinary ask-mode gate.
	gateOptions permissions.Options
	// respond builds the operator's turn-2 answer; nil leaves the run
	// parked.
	respond func(confID string) *genai.Content
	// seed appends events to the session before the run — used to put
	// the outbox into ambiguous-effect mode.
	seed func(t *testing.T, svc adksession.Service)
	// extraPluginsFirst are registered ahead of the write gate.
	extraPluginsFirst func(t *testing.T) []*plugin.Plugin
	// restartBeforeVerdict builds a completely fresh runner, plugin,
	// gate and session-service handle for the operator's turn, keeping
	// only the on-disk event log. It stands in for the process dying
	// while an operator thinks (scoreboard row 5).
	restartBeforeVerdict bool
}

func runGateProbe(t *testing.T, cfg gateProbeConfig) *gateProbe {
	t.Helper()
	probe := &gateProbe{gate: permissions.New(cfg.gateOptions)}
	dir := t.TempDir()

	// build assembles one "process": agent, plugins, runner, and a fresh
	// handle on the shared store. Called twice when restartBeforeVerdict
	// is set, so nothing but the event log crosses the restart.
	build := func() (*runner.Runner, adksession.Service) {
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

		m := &scriptedModel{name: "gate", calls: []*model.LLMResponse{{
			Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", map[string]any{
					"deployment": "api",
					"replicas":   10,
				})},
			},
		}}}

		root, err := llmagent.New(llmagent.Config{
			Name:        "gate_agent",
			Description: "write-gate probe",
			Instruction: "act",
			Model:       m,
			Tools:       []tool.Tool{scale},
		})
		if err != nil {
			t.Fatalf("llmagent.New: %v", err)
		}

		mutating := cfg.mutating
		if mutating == nil {
			mutating = alwaysMutating
		}
		wg, err := New(Config{
			Policy:   cfg.policy,
			Mutating: mutating,
			Gate:     probe.gate,
		})
		if err != nil {
			t.Fatalf("approval.New: %v", err)
		}

		var plugins []*plugin.Plugin
		if cfg.extraPluginsFirst != nil {
			plugins = append(plugins, cfg.extraPluginsFirst(t)...)
		}
		plugins = append(plugins, wg)

		svc := sqliteServiceAt(t, dir)
		r, err := runner.New(runner.Config{
			AppName:           testApp,
			Agent:             root,
			SessionService:    svc,
			AutoCreateSession: true,
			PluginConfig:      runner.PluginConfig{Plugins: plugins},
		})
		if err != nil {
			t.Fatalf("runner.New: %v", err)
		}
		return r, svc
	}

	run := func(r *runner.Runner, msg *genai.Content) {
		t.Helper()
		for _, err := range r.Run(context.Background(), testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("runner.Run: %v", err)
			}
		}
	}

	r, svc := build()
	if cfg.seed != nil {
		// AutoCreateSession only fires on Run, so the session has to
		// exist before anything can be appended to it.
		if _, err := svc.Create(context.Background(), &adksession.CreateRequest{
			AppName: testApp, UserID: testUser, SessionID: sid,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		cfg.seed(t, svc)
	}

	run(r, genai.NewContentFromText("scale api to 10", genai.RoleUser))
	probe.confirmationID, probe.hintSeen = confirmationRequest(t, svc)

	if cfg.restartBeforeVerdict {
		if got := probe.gate.Approvals(); len(got) != 0 {
			t.Fatalf("the pre-restart gate already recorded an approval: %+v", got)
		}
		probe.gate = permissions.New(cfg.gateOptions)
		r, svc = build()
	}
	if cfg.respond != nil && probe.confirmationID != "" {
		run(r, cfg.respond(probe.confirmationID))
	}
	probe.responses = toolResponses(t, svc, "scale_deployment")
	return probe
}

// toolResponses collects, in log order, every FunctionResponse recorded
// for the named tool. This is the model's view of what happened, which
// is the thing the gate's refusal wording has to get right.
func toolResponses(t *testing.T, svc adksession.Service, name string) []map[string]any {
	t.Helper()
	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var out []map[string]any
	for ev := range got.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part == nil || part.FunctionResponse == nil || part.FunctionResponse.Name != name {
				continue
			}
			out = append(out, part.FunctionResponse.Response)
		}
	}
	return out
}

// lastResponse is the tool response the model ended up with.
func (p *gateProbe) lastResponse(t *testing.T) map[string]any {
	t.Helper()
	if len(p.responses) == 0 {
		t.Fatal("no tool response recorded for scale_deployment")
	}
	return p.responses[len(p.responses)-1]
}

func wantField(t *testing.T, resp map[string]any, field, want string) {
	t.Helper()
	got, _ := resp[field].(string)
	if got != want {
		t.Errorf("response[%q] = %q, want %q (full response: %v)", field, got, want, resp)
	}
}

func wantDetailMentions(t *testing.T, resp map[string]any, substrings ...string) {
	t.Helper()
	detail, _ := resp["detail"].(string)
	for _, s := range substrings {
		if !strings.Contains(detail, s) {
			t.Errorf("detail does not mention %q: %s", s, detail)
		}
	}
}

func approve(payload map[string]any) func(string) *genai.Content {
	return func(confID string) *genai.Content {
		return verdictResponse(confID, map[string]any{"confirmed": true, "payload": payload})
	}
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"empty policy", Config{Mutating: alwaysMutating}, "is not one of"},
		{"unknown policy", Config{Policy: "ask_nicely", Mutating: alwaysMutating}, "is not one of"},
		{"no predicate", Config{Policy: OnMutationApply}, "Mutating is required"},
		{
			// The reason this validation exists: no permissions.Gate is
			// constructed anywhere in mast's non-test code today, so a
			// require_approval workload wired by an inattentive caller
			// would otherwise get a gate that approves everything.
			"require_approval without a gate",
			Config{Policy: OnMutationRequireApproval, Mutating: alwaysMutating},
			"Gate is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tt.cfg)
			if err == nil {
				t.Fatal("New = nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// TestReadOnlyToolIsNotGated: the gate is invisible to everything the
// mutation predicate does not flag. A triage agent reading pod logs must
// not wake an operator.
func TestReadOnlyToolIsNotGated(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:   OnMutationRequireApproval,
		mutating: func(string) bool { return false },
	})
	if p.confirmationID != "" {
		t.Error("a read-only tool parked for approval")
	}
	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
}

// TestRequireApprovalParksTheCall is scoreboard row 4's core assertion:
// the call stops BEFORE it fires, and the model is told to stop rather
// than to try something else.
func TestRequireApprovalParksTheCall(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationRequireApproval})

	if p.confirmationID == "" {
		t.Fatal("no confirmation parked")
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) while parked, want 0: %+v", len(p.executions), p.executions)
	}
	if want := "Approve mutating call scale_deployment(deployment=api, replicas=10)?"; p.hintSeen != want {
		t.Errorf("hint = %q, want %q", p.hintSeen, want)
	}
	resp := p.lastResponse(t)
	wantField(t, resp, "status", "awaiting_operator_approval")
	// A confirmation request does not halt the agent loop — the model
	// reads this response and keeps going — so the wording has to do the
	// work of stopping it. E-approval-rejected tests the same property
	// against a live model.
	wantDetailMentions(t, resp, "has NOT been made", "Do not retry", "another route")
}

func TestApprovedCallRunsExactlyOnce(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:  OnMutationRequireApproval,
		respond: approve(map[string]any{"verdict": "approve", "approver": "user:sre-oncall"}),
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want exactly 1: %+v", len(p.executions), p.executions)
	}
	if got := p.executions[0]; got.Deployment != "api" || got.Replicas != 10 {
		t.Errorf("executed with %+v, want {api 10}", got)
	}
	log := p.gate.Approvals()
	if len(log) != 1 {
		t.Fatalf("approval log = %+v, want exactly one entry", log)
	}
	if log[0].Tool != "scale_deployment" || log[0].Decision != permissions.DecisionAllowOnce {
		t.Errorf("approval log entry = %+v, want an allow-once for scale_deployment", log[0])
	}
	if log[0].Key != "scale_deployment(deployment=api, replicas=10)" {
		t.Errorf("approval key = %q, want the rendered call — an audit entry that does not say what was approved is not an audit entry", log[0].Key)
	}
}

// TestPauseSurvivesARestart is scoreboard row 5 in miniature: nothing
// but the on-disk event log crosses from the turn that parked the call
// to the turn that approves it — new runner, new plugin instance, new
// permissions gate, new handle on the store. This is why the pause is
// built on ADK's confirmation flow and not on permissions.Prompter,
// which holds the question in a goroutine that a restart destroys.
//
// The full-fidelity version (kill -9 an actual daemon) is U-gate-crash
// in the UAT; this one runs in CI on every commit.
func TestPauseSurvivesARestart(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:               OnMutationRequireApproval,
		restartBeforeVerdict: true,
		respond:              approve(map[string]any{"verdict": "approve", "approver": "user:sre-oncall"}),
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s) across the restart, want exactly 1: %+v", len(p.executions), p.executions)
	}
	if got := p.executions[0]; got.Deployment != "api" || got.Replicas != 10 {
		t.Errorf("executed with %+v, want the parked call's arguments {api 10}", got)
	}
	if log := p.gate.Approvals(); len(log) != 1 {
		t.Errorf("post-restart approval log = %+v, want the one approval this process granted", log)
	}
}

func TestRejectedCallNeverRuns(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: func(confID string) *genai.Content {
			return verdictResponse(confID, map[string]any{
				"confirmed": false,
				"payload":   map[string]any{"verdict": "reject", "note": "wrong deployment", "approver": "user:sre-oncall"},
			})
		},
	})

	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) after a rejection, want 0: %+v", len(p.executions), p.executions)
	}
	resp := p.lastResponse(t)
	wantField(t, resp, "error", "denied_by_operator")
	wantField(t, resp, "operator_note", "wrong deployment")
	wantField(t, resp, "approver", "user:sre-oncall")
	wantDetailMentions(t, resp, "Do not retry", "another route")
	if log := p.gate.Approvals(); len(log) != 0 {
		t.Errorf("approval log = %+v, want empty after a rejection", log)
	}
}

// TestBroadScopeIsRefused is U-gate-scopes at the seam: a client that
// asks to authorize more than this one call gets nothing, not a
// narrowed grant.
func TestBroadScopeIsRefused(t *testing.T) {
	for _, scope := range []Scope{ScopeSession, ScopeSessionTool, ScopeAlways} {
		t.Run(string(scope), func(t *testing.T) {
			p := runGateProbe(t, gateProbeConfig{
				policy: OnMutationRequireApproval,
				// yolo mode as well, so the test also says that the most
				// permissive process-level setting mast has does not
				// rescue an inadmissible scope.
				gateOptions: permissions.Options{Mode: permissions.ModeYolo},
				respond:     approve(map[string]any{"verdict": "approve", "scope": string(scope)}),
			})
			if len(p.executions) != 0 {
				t.Fatalf("tool executed %d time(s) under scope %s, want 0: %+v", len(p.executions), scope, p.executions)
			}
			wantField(t, p.lastResponse(t), "error", "approval_scope_refused")
			if log := p.gate.Approvals(); len(log) != 0 {
				t.Errorf("approval log = %+v, want empty after a refused scope", log)
			}
		})
	}
}

func TestUnknownScopeIsRefused(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:  OnMutationRequireApproval,
		respond: approve(map[string]any{"verdict": "approve", "scope": "forever_and_ever"}),
	})
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) under an unknown scope, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "malformed_verdict")
}

// TestEditIsRefusedUntilW25 pins the deliberate gap: the wire format
// takes an edit today so clients need not change later, and mast says
// plainly that it did not apply it. The seam probes show the mechanism
// works; what W2.5 still owes is validating the operator's arguments
// against the tool's input schema.
func TestEditIsRefusedUntilW25(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: approve(map[string]any{
			"verdict": "edit",
			"args":    map[string]any{"deployment": "api", "replicas": 2},
		}),
	})
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) on an edit verdict, want 0: %+v", len(p.executions), p.executions)
	}
	resp := p.lastResponse(t)
	wantField(t, resp, "error", "not_implemented")
	wantDetailMentions(t, resp, "was NOT made")
}

// TestContradictoryVerdictIsRefused: a payload that says approve under
// ADK's confirmed=false is a serialization bug in the caller, and
// picking a winner would be picking whether to mutate a cluster.
func TestContradictoryVerdictIsRefused(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: func(confID string) *genai.Content {
			return verdictResponse(confID, map[string]any{
				"confirmed": false,
				"payload":   map[string]any{"verdict": "approve"},
			})
		},
	})
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) on a contradictory verdict, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "malformed_verdict")
}

// TestPolicyDenyIsNotPutToAnOperator: a configured deny is settled. The
// gate must not park it, because parking it invites an operator to
// override a rule they wrote precisely so they would not be asked.
func TestPolicyDenyIsNotPutToAnOperator(t *testing.T) {
	policy, err := permissions.NewPolicy(nil, []string{"scale_deployment:*"})
	if err != nil {
		t.Fatal(err)
	}
	p := runGateProbe(t, gateProbeConfig{
		policy:      OnMutationRequireApproval,
		gateOptions: permissions.Options{Mode: permissions.ModeYolo, Policy: policy},
	})
	if p.confirmationID != "" {
		t.Error("a denied call was parked for approval")
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) despite a deny rule, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "denied_by_policy")
}

func TestApplyPolicyExecutesWithoutAsking(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationApply})
	if p.confirmationID != "" {
		t.Error("apply parked for approval")
	}
	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
}

func TestDryRunPolicyNeverExecutes(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationDryRun})
	if p.confirmationID != "" {
		t.Error("dry_run parked for approval")
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) under dry_run, want 0: %+v", len(p.executions), p.executions)
	}
	resp := p.lastResponse(t)
	wantField(t, resp, "status", "dry_run")
	wantField(t, resp, "tool", "scale_deployment")
	// The model must be able to report the proposal, so the arguments
	// come back with it.
	args, _ := resp["args"].(map[string]any)
	if args == nil || args["deployment"] != "api" {
		t.Errorf("response args = %v, want the proposed call's arguments", resp["args"])
	}
	wantDetailMentions(t, resp, "NOT made", "nothing changed")
}

// TestOutboxRunsBeforeTheGate is resolved-decision row 144 as a test.
// The outbox is registered first, so when it refuses a call the write
// gate never sees it — no operator is asked to approve a mutation whose
// prior outcome is unknown, which is a question they cannot answer
// correctly anyway.
//
// ADK runs before-tool callbacks in registration order and takes the
// first non-nil response, so this ordering is the whole mechanism; the
// test fails if a construction site ever registers them the other way.
func TestOutboxRunsBeforeTheGate(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		seed:   seedDanglingMutation,
		extraPluginsFirst: func(t *testing.T) []*plugin.Plugin {
			t.Helper()
			outbox, err := effects.New(effects.Config{Predicate: effects.NewPredicate(nil)})
			if err != nil {
				t.Fatalf("effects.New: %v", err)
			}
			return []*plugin.Plugin{outbox}
		},
	})

	if p.confirmationID != "" {
		t.Error("the write gate parked a call the outbox had already refused")
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) in ambiguous-effect mode, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "ambiguous_prior_effect")
}

// seedDanglingMutation writes an unpaired mutating FunctionCall into the
// log — the wire shape of a turn that died between calling a tool and
// recording what it returned.
func seedDanglingMutation(t *testing.T, svc adksession.Service) {
	t.Helper()
	call := genai.NewPartFromFunctionCall("scale_deployment", map[string]any{"deployment": "api", "replicas": 3})
	call.FunctionCall.ID = "dangling-call-1"
	ev := &adksession.Event{
		InvocationID: "prior-invocation",
		Author:       "gate_agent",
		Timestamp:    time.Now().Add(-time.Minute),
		LLMResponse:  model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{call}}},
	}
	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := svc.AppendEvent(context.Background(), got.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func TestCallKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no args", nil, "restart_daemonset()"},
		{
			// Sorted, not map-iteration order: an audit key that renders
			// differently run to run cannot be matched by a deny pattern
			// or compared across two approvals.
			"keys are sorted",
			map[string]any{"replicas": 10, "deployment": "api", "namespace": "prod"},
			"restart_daemonset(deployment=api, namespace=prod, replicas=10)",
		},
		{"nested values are compact json", map[string]any{"patch": map[string]any{"spec": true}}, `restart_daemonset(patch={"spec":true})`},
		{"long values are elided", map[string]any{"manifest": strings.Repeat("y", 200)}, "restart_daemonset(manifest=" + strings.Repeat("y", 120) + "…)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CallKey("restart_daemonset", tt.args); got != tt.want {
				t.Errorf("CallKey = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCallKeyIsStableAcrossRenders(t *testing.T) {
	t.Parallel()
	args := map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8}
	first := CallKey("t", args)
	for i := 0; i < 50; i++ {
		if got := CallKey("t", args); got != first {
			t.Fatalf("CallKey render %d = %q, want %q", i, got, first)
		}
	}
}

func TestDecodeVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		c       *toolconfirmation.ToolConfirmation
		want    Verdict
		wantErr string
	}{
		{
			// A client that speaks only ADK's boolean still works.
			name: "bare confirmed",
			c:    &toolconfirmation.ToolConfirmation{Confirmed: true},
			want: Verdict{Verdict: OutcomeApprove},
		},
		{
			name: "bare denial",
			c:    &toolconfirmation.ToolConfirmation{Confirmed: false},
			want: Verdict{Verdict: OutcomeReject},
		},
		{
			name: "full record through the event log's map shape",
			c: &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: map[string]any{
				"verdict":  "approve",
				"scope":    "once",
				"note":     "checked the HPA first",
				"approver": "user:sre-oncall",
			}},
			want: Verdict{Verdict: OutcomeApprove, Scope: ScopeOnce, Note: "checked the HPA first", Approver: "user:sre-oncall"},
		},
		{
			name: "typed payload in-process",
			c:    &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: Verdict{Verdict: OutcomeApprove, Approver: "user:x"}},
			want: Verdict{Verdict: OutcomeApprove, Approver: "user:x"},
		},
		{
			name: "edit carries replacement arguments",
			c: &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: map[string]any{
				"verdict": "edit",
				"args":    map[string]any{"replicas": float64(2)},
			}},
			want: Verdict{Verdict: OutcomeEdit, Args: map[string]any{"replicas": float64(2)}},
		},
		{
			name:    "unknown verdict",
			c:       &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: map[string]any{"verdict": "maybe"}},
			wantErr: "unknown verdict",
		},
		{
			name:    "approve contradicting confirmed=false",
			c:       &toolconfirmation.ToolConfirmation{Confirmed: false, Payload: map[string]any{"verdict": "approve"}},
			wantErr: "contradictory",
		},
		{
			name:    "reject contradicting confirmed=true",
			c:       &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: map[string]any{"verdict": "reject"}},
			wantErr: "contradictory",
		},
		{
			name:    "payload that is not a record",
			c:       &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: "looks good to me"},
			wantErr: "not a verdict record",
		},
		{
			name:    "no confirmation at all",
			c:       nil,
			wantErr: "no tool confirmation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeVerdict(tt.c)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("DecodeVerdict = %+v, want error mentioning %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeVerdict: %v", err)
			}
			if got.Verdict != tt.want.Verdict || got.Scope != tt.want.Scope ||
				got.Note != tt.want.Note || got.Approver != tt.want.Approver ||
				len(got.Args) != len(tt.want.Args) {
				t.Errorf("DecodeVerdict = %+v, want %+v", got, tt.want)
			}
			for k, want := range tt.want.Args {
				if got.Args[k] != want {
					t.Errorf("args[%q] = %v, want %v", k, got.Args[k], want)
				}
			}
		})
	}
}

func TestVerdictDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v    Verdict
		want permissions.Decision
	}{
		{Verdict{Verdict: OutcomeApprove}, permissions.DecisionAllowOnce},
		{Verdict{Verdict: OutcomeApprove, Scope: ScopeOnce}, permissions.DecisionAllowOnce},
		{Verdict{Verdict: OutcomeApprove, Scope: ScopeSessionTool}, permissions.DecisionAllowSessionTool},
		// A reject is a reject at any scope: there is no "deny this for
		// the whole session" that means anything different.
		{Verdict{Verdict: OutcomeReject, Scope: ScopeAlways}, permissions.DecisionDeny},
	}
	for _, tt := range tests {
		got, err := tt.v.Decision()
		if err != nil {
			t.Fatalf("Decision() for %+v: %v", tt.v, err)
		}
		if got != tt.want {
			t.Errorf("Decision() for %+v = %v, want %v", tt.v, got, tt.want)
		}
	}
}
