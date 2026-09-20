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
	"errors"
	"iter"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/permissions"
)

// proposingModel answers each round with one scale_deployment call,
// taking that round's arguments from calls. A nil entry — and anything
// past the end of the script — is plain text, which ends the turn.
//
// The existing scriptedModel cannot stand in for this: it falls through
// to "done" after its single scripted round, and the behaviour under
// test here is entirely about what a model does on the rounds AFTER an
// operator has answered.
type proposingModel struct {
	mu    sync.Mutex
	round int
	calls []map[string]any
}

func (m *proposingModel) Name() string { return "refusal-loop" }

func (m *proposingModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		round := m.round
		m.round++
		m.mu.Unlock()

		var args map[string]any
		if round < len(m.calls) {
			args = m.calls[round]
		}
		resp := &model.LLMResponse{TurnComplete: true, FinishReason: genai.FinishReasonStop}
		if args == nil {
			resp.Content = genai.NewContentFromText("done", genai.RoleModel)
		} else {
			resp.Content = &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", args)},
			}
		}
		yield(resp, nil)
	}
}

func proposeScale(deployment string, replicas int) map[string]any {
	return map[string]any{"deployment": deployment, "replicas": replicas}
}

// refusalProbe is one session driven through a park, an operator's
// answer, and whatever the model does next — which is the part that
// matters here.
type refusalProbe struct {
	// executions is one entry per actual tool execution.
	executions []scaleArgs
	// responses is what the model was told, in order, for every
	// scale_deployment call.
	responses []map[string]any
	// parks is every confirmation the gate opened, in log order. The
	// count is the headline number of #449: a refusal the model will not
	// accept must not turn into a second question for the operator.
	parks []string
	// invocations is the invocation ID seen at each scale_deployment
	// call, in order, recorded by a plugin registered ahead of the gate.
	invocations []string
	// forgotten is every session ID handed to Config.ForgetToolRun.
	forgotten []string
	// errs is every error a run yielded, across all turns.
	errs []error
	// stop is the host side of the turn-end: the reason the gate handed
	// it, and the live run's cancel handle.
	stop *probeStop
}

// probeStop stands in for cmd/mast's refusalStop: hold the reason,
// cancel the run. Written here rather than mocked because the thing
// under test is that cancelling from inside a before-tool callback
// actually stops the flow — a mock that only records the call would
// have passed against the version of this feature that did not work.
type probeStop struct {
	mu     sync.Mutex
	reason error
	cancel context.CancelFunc
}

func (s *probeStop) StopTurn(reason error) {
	s.mu.Lock()
	if s.reason == nil {
		s.reason = reason
	}
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *probeStop) arm(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = cancel
}

func (s *probeStop) why() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

type refusalProbeConfig struct {
	// calls is the model's script, one entry per round.
	calls []map[string]any
	// respond builds the operator's answer to the first park; nil leaves
	// the run parked.
	respond func(confID string) *genai.Content
	// thirdTurn, when non-nil, is a fresh operator message sent after the
	// verdict turn has finished. A fresh turn is the only way to observe
	// the boundary this whole mechanism is keyed on.
	thirdTurn *genai.Content
	// noForget drops Config.ForgetToolRun, standing in for a composition
	// with no watchdog to tell.
	noForget bool
	// noStop installs no TurnStop on the run context, standing in for a
	// library embed that owns its own loop.
	noStop bool
}

func runRefusalProbe(t *testing.T, cfg refusalProbeConfig) *refusalProbe {
	t.Helper()
	probe := &refusalProbe{stop: &probeStop{}}

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

	root, err := llmagent.New(llmagent.Config{
		Name:        "refusal_agent",
		Description: "refusal-loop probe",
		Instruction: "act",
		Model:       &proposingModel{calls: cfg.calls},
		Tools:       []tool.Tool{scale},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	// Registered ahead of the gate so it sees every call the gate sees,
	// including the ones the gate then turns away. Returning nil lets the
	// gate run: ADK takes the first non-nil response.
	observer, err := plugin.New(plugin.Config{
		Name: "invocation-observer",
		BeforeToolCallback: func(ctx adkagent.Context, tl tool.Tool, _ map[string]any) (map[string]any, error) {
			if tl.Name() == "scale_deployment" {
				probe.invocations = append(probe.invocations, ctx.InvocationID())
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("plugin.New: %v", err)
	}

	gcfg := Config{
		Policy:   OnMutationRequireApproval,
		Mutating: alwaysMutating,
		Gate:     permissions.New(permissions.Options{}),
	}
	if !cfg.noForget {
		gcfg.ForgetToolRun = func(sessionID string) {
			probe.forgotten = append(probe.forgotten, sessionID)
		}
	}
	wg, err := New(gcfg)
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}

	svc := sqliteService(t)
	r, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{observer, wg}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	// Each turn gets its own cancellable context, exactly as the daemon
	// gives one to each turn, and the stop is armed with that turn's
	// handle. Unlike the other probes in this package, a run here is
	// EXPECTED to be cut short on one path — that is the feature — so
	// errors are collected rather than fatal.
	run := func(msg *genai.Content) {
		runCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		probe.stop.arm(cancel)
		if !cfg.noStop {
			runCtx = WithTurnStop(runCtx, probe.stop)
		}
		for _, err := range r.Run(runCtx, testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil {
				probe.errs = append(probe.errs, err)
			}
		}
	}

	run(genai.NewContentFromText("scale api to 10", genai.RoleUser))

	ids, _, _ := confirmationRequests(t, svc)
	if cfg.respond != nil && len(ids) > 0 {
		run(cfg.respond(ids[len(ids)-1]))
	}
	if cfg.thirdTurn != nil {
		run(cfg.thirdTurn)
	}

	probe.parks, _, _ = confirmationRequests(t, svc)
	probe.responses = toolResponses(t, svc, "scale_deployment")
	return probe
}

func reject(confID string) *genai.Content {
	return verdictResponse(confID, map[string]any{
		"confirmed": false,
		"payload":   map[string]any{"verdict": "reject", "approver": "operator@example.com"},
	})
}

// TestARefusedCallIsNotPutToTheOperatorTwice is #449's core assertion
// and the one that fails before the change: the model re-proposes the
// call byte for byte after being told no, and the gate answers it
// instead of opening a second park.
//
// A second park is not a cosmetic repeat. Each one writes a fresh
// long-running call into the durable event log and a fresh row into GET
// /parks, and pages whoever answered the first one.
func TestARefusedCallIsNotPutToTheOperatorTwice(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10), // parks
			proposeScale("api", 10), // after the refusal: the same call again
			nil,                     // then the model gives up
		},
		respond: reject,
	})

	if len(p.parks) != 1 {
		t.Fatalf("operator was asked %d times, want 1 — a refusal they already gave was put to them again", len(p.parks))
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) after a refusal, want 0: %+v", len(p.executions), p.executions)
	}
	if len(p.errs) != 0 {
		t.Fatalf("run errors = %v, want none — one suppression is not a loop", p.errs)
	}
	if len(p.responses) < 2 {
		t.Fatalf("responses = %v, want at least the park and the suppression", p.responses)
	}
	last := p.responses[len(p.responses)-1]
	wantField(t, last, "error", "already_refused")
	wantDetailMentions(t, last,
		"already refused",
		"not put to them a second time",
		"Do not retry",
	)
}

// TestADifferentCallStillParks is the other half of the same mechanism.
// The refusal is evidence about the call that was refused and nothing
// else: a model that reads the refusal and fixes its arguments is doing
// what the refusal text asks for, and must still get an operator.
func TestADifferentCallStillParks(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10), // parks
			proposeScale("api", 2),  // a different call: a different question
		},
		respond: reject,
	})

	if len(p.parks) != 2 {
		t.Fatalf("operator was asked %d times, want 2 — a call they never saw was refused on their behalf", len(p.parks))
	}
	for _, resp := range p.responses {
		if code, _ := resp["error"].(string); code == "already_refused" {
			t.Fatalf("a call the operator never refused was suppressed: %v", resp)
		}
	}
}

// TestAnApprovalDoesNotArmTheSuppression: only a no is remembered.
// Saying yes to one invocation of a mutating call is not evidence about
// the next one in either direction — a second scale_deployment may be a
// second legitimate change — so the approved call runs and the repeat
// parks like any other.
func TestAnApprovalDoesNotArmTheSuppression(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10), // parks
			proposeScale("api", 10), // the same call again, this time after a yes
		},
		respond: approve(map[string]any{"verdict": "approve", "approver": "operator@example.com"}),
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want exactly 1 — the approved call: %+v", len(p.executions), p.executions)
	}
	if len(p.parks) != 2 {
		t.Fatalf("operator was asked %d times, want 2 — an approval armed the refusal memory", len(p.parks))
	}
	for _, resp := range p.responses {
		if code, _ := resp["error"].(string); code == "already_refused" {
			t.Fatalf("an approval was remembered as a refusal: %v", resp)
		}
	}
}

// TestTheTurnBoundaryIsTheInvocationID is the measurement suppress.go's
// design rests on, kept as a test because it is a fact about ADK that
// ADK does not document and could change under us.
//
// Two claims, and the mechanism needs both. The park, the operator's
// verdict and everything the model does after being told the answer all
// carry ONE invocation ID — even though the verdict arrives on a
// separate Runner.Run — so a refusal recorded at the park is still in
// scope when the model re-proposes. And a fresh operator turn gets a new
// one, so the memory expires without any clearing step: the same call,
// proposed in the next turn, parks again.
func TestTheTurnBoundaryIsTheInvocationID(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10), // turn 1: parks
			proposeScale("api", 10), // turn 1, post-verdict: suppressed
			nil,                     // turn 1 ends
			proposeScale("api", 10), // turn 2: the same call, a new turn
		},
		respond:   reject,
		thirdTurn: genai.NewContentFromText("try scaling api again", genai.RoleUser),
	})

	if len(p.invocations) != 4 {
		t.Fatalf("saw %d tool calls %v, want 4: park, verdict, suppression, next turn", len(p.invocations), p.invocations)
	}
	for i, got := range p.invocations[:3] {
		if got != p.invocations[0] {
			t.Fatalf("tool call %d ran under invocation %q, want %q — the park, the verdict and the re-proposal are not one turn, so a refusal cannot be scoped to one",
				i, got, p.invocations[0])
		}
	}
	if p.invocations[3] == p.invocations[0] {
		t.Fatalf("a fresh operator turn reused invocation %q — the refusal memory would never expire", p.invocations[3])
	}

	if len(p.parks) != 2 {
		t.Fatalf("operator was asked %d times, want 2 — the first turn's refusal is still suppressing calls in the second", len(p.parks))
	}
	last := p.responses[len(p.responses)-1]
	if got, _ := last["status"].(string); got != "awaiting_operator_approval" {
		t.Fatalf("the new turn's call ended as %v, want a fresh park — a refusal outlived its turn", last)
	}
}

// TestThreeSuppressedCallsEndTheTurn: answering a model that will not
// stop is free, so the answer alone does not bound anything. The turn
// does.
//
// The script offers the same call twice more than the threshold allows,
// so the count of calls the gate actually saw is the assertion that the
// turn stopped rather than merely said it had.
func TestThreeSuppressedCallsEndTheTurn(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10), // parks
			proposeScale("api", 10), // suppressed 1
			proposeScale("api", 10), // suppressed 2
			proposeScale("api", 10), // suppressed 3 — the turn ends here
			proposeScale("api", 10), // never reached
			proposeScale("api", 10), // never reached
		},
		respond: reject,
	})

	var loop *RefusalLoopError
	if !errors.As(p.stop.why(), &loop) {
		t.Fatalf("the turn ended with %v, want a *RefusalLoopError — nothing stopped a model that would not stop", p.stop.why())
	}
	if loop.Count != suppressedCallsEndTurn {
		t.Errorf("Count = %d, want %d", loop.Count, suppressedCallsEndTurn)
	}
	if loop.Tool != "scale_deployment" {
		t.Errorf("Tool = %q, want scale_deployment — the error names the wrong call", loop.Tool)
	}

	// Park, verdict, and the three suppressions. A sixth means the flow
	// went round again after the gate said it was done, which is what
	// returning an error from the callback actually does.
	if len(p.invocations) != 5 {
		t.Errorf("the gate saw %d calls, want 5 (park, verdict, %d suppressions) — the turn did not stop", len(p.invocations), suppressedCallsEndTurn)
	}
	if len(p.parks) != 1 {
		t.Errorf("operator was asked %d times, want 1", len(p.parks))
	}
	if len(p.executions) != 0 {
		t.Errorf("tool executed %d time(s), want 0: %+v", len(p.executions), p.executions)
	}
}

// TestTheTurnStopIsOptional: a library embed drives its own loop and
// would not want a dependency cancelling it. The suppression is the half
// that protects the operator and it must work with no host to stop the
// turn — the gate simply goes on refusing.
func TestTheTurnStopIsOptional(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10),
			proposeScale("api", 10),
			proposeScale("api", 10),
			proposeScale("api", 10),
			nil,
		},
		respond: reject,
		noStop:  true,
	})

	if p.stop.why() != nil {
		t.Errorf("a turn with no TurnStop recorded %v", p.stop.why())
	}
	if len(p.parks) != 1 {
		t.Errorf("operator was asked %d times, want 1 — the suppression depends on the stop", len(p.parks))
	}
	if len(p.executions) != 0 {
		t.Errorf("tool executed %d time(s), want 0: %+v", len(p.executions), p.executions)
	}
	last := p.responses[len(p.responses)-1]
	wantField(t, last, "error", "already_refused")
}

// TestTheSuppressionTellsTheWatchdogItsCallsAreDisposedOf is the
// correction at core-agent@4095b15a, in mast's own shape. Without it the
// gate's suppressed re-proposals are still on the watchdog's books as a
// run of identical calls, and the session trips at five anyway — so a
// refusal the gate handled at turn scope would still cost an operator a
// guardrail reset.
func TestTheSuppressionTellsTheWatchdogItsCallsAreDisposedOf(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10),
			proposeScale("api", 10), // suppressed 1
			proposeScale("api", 10), // suppressed 2
			nil,
		},
		respond: reject,
	})

	if len(p.forgotten) != 2 {
		t.Fatalf("ForgetToolRun called %d time(s) %v, want one per suppressed call (2)", len(p.forgotten), p.forgotten)
	}
	for _, got := range p.forgotten {
		if got != sid {
			t.Errorf("ForgetToolRun(%q), want the session under the loop (%q)", got, sid)
		}
	}
}

// TestSuppressionWorksWithNoWatchdogToTell: a library embed and a
// one-shot have no watchdog, and the gate's own stop must not depend on
// one being there.
func TestSuppressionWorksWithNoWatchdogToTell(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls:    []map[string]any{proposeScale("api", 10), proposeScale("api", 10), nil},
		respond:  reject,
		noForget: true,
	})

	if len(p.parks) != 1 {
		t.Fatalf("operator was asked %d times, want 1", len(p.parks))
	}
	last := p.responses[len(p.responses)-1]
	wantField(t, last, "error", "already_refused")
}

// TestRefusalLoopErrorDeclaresItsTurnErrorKind pins the bare-string
// contract from the side that owns the string. SelfClassifyingError
// carries a kind and nothing else so a raiser need not import
// pkg/attach; the price of the weaker contract is this assertion, which
// is the same one pkg/watchdog pays for TrippedError.
//
// A kind pkg/attach does not ship falls back to substring-scanning the
// error text, and this error's text mentions a refusal and a tool name —
// which is exactly how #208's halts came out as config_error.
func TestRefusalLoopErrorDeclaresItsTurnErrorKind(t *testing.T) {
	t.Parallel()
	var err error = &RefusalLoopError{Tool: "patch_resource", Key: "patch_resource(name=api)", Count: 3}

	var sce attach.SelfClassifyingError
	if !errors.As(err, &sce) {
		t.Fatalf("%T does not implement attach.SelfClassifyingError", err)
	}
	if got := sce.TurnErrorKind(); got != attach.TurnErrorRefusalLoop {
		t.Fatalf("TurnErrorKind() = %q, want attach.TurnErrorRefusalLoop (%q)", got, attach.TurnErrorRefusalLoop)
	}

	te := attach.ClassifyTurnError(err)
	if te.Kind != attach.TurnErrorRefusalLoop {
		t.Errorf("ClassifyTurnError kind = %q, want %q", te.Kind, attach.TurnErrorRefusalLoop)
	}
	if te.Retryable {
		t.Error("Retryable = true — a retry re-proposes the call the operator refused")
	}
}

// The unit-level properties of the memory itself, which the end-to-end
// tests above cannot reach: an embed that drives the callback by hand
// has no invocation to key on, and the gate must degrade to its old
// behaviour rather than to a suppression that can never expire.
func TestRefusalsWithoutATurnIdentityDoNothing(t *testing.T) {
	t.Parallel()
	r := newRefusals()
	r.remember("", "scale_deployment(replicas=10)")
	if suppressed, _, _ := r.suppress("", "scale_deployment(replicas=10)"); suppressed {
		t.Error("suppressed a call with no turn to scope the refusal to")
	}
	if len(r.byInvocation) != 0 {
		t.Errorf("byInvocation = %v, want empty", r.byInvocation)
	}
}

func TestRefusalsCountTheTurnNotTheCall(t *testing.T) {
	t.Parallel()
	r := newRefusals()
	for _, key := range []string{"a()", "b()", "c()"} {
		r.remember("inv-1", key)
	}
	// Three different refused calls in rotation is the same failure as
	// one call three times, and only a turn-level count catches it.
	for i, key := range []string{"a()", "b()", "c()"} {
		suppressed, endTurn, count := r.suppress("inv-1", key)
		if !suppressed {
			t.Fatalf("%s was not suppressed", key)
		}
		if count != i+1 {
			t.Errorf("count = %d, want %d", count, i+1)
		}
		if want := i+1 >= suppressedCallsEndTurn; endTurn != want {
			t.Errorf("endTurn = %v after %d suppressions, want %v", endTurn, count, want)
		}
	}
}

func TestForgetDropsOnlyItsOwnTurn(t *testing.T) {
	t.Parallel()
	r := newRefusals()
	r.remember("inv-1", "a()")
	r.remember("inv-2", "a()")
	r.forget("inv-1")

	if suppressed, _, _ := r.suppress("inv-1", "a()"); suppressed {
		t.Error("a forgotten turn still suppresses")
	}
	if suppressed, _, _ := r.suppress("inv-2", "a()"); !suppressed {
		t.Error("forgetting one turn dropped another's refusal")
	}
}
