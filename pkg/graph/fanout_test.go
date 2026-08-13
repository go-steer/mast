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
	"iter"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// readOnly is the allowlist an analyst has to declare to be buildable:
// a named server with its tools enumerated by name. The workload has to
// classify those names read-only for the build to succeed — see
// catalogBundle.
func readOnly(tools ...string) specialists.ToolAllowlist {
	return specialists.ToolAllowlist{
		MCP: []specialists.MCPAllowlist{{Server: "gke", Tools: tools}},
	}
}

func boolp(b bool) *bool { return &b }

// catalogBundle is a workload with an MCP catalog and read-only
// classifications for the named tools — the shipped ns-audit shape in
// miniature.
func catalogBundle(readOnlyTools ...string) workload.Bundle {
	b := workload.Bundle{
		Name:        "w",
		ToolCatalog: workload.ToolCatalog{MCP: []workload.MCPServerRef{{Server: "gke"}}},
	}
	for _, name := range readOnlyTools {
		b.ToolCatalog.Tools = append(b.ToolCatalog.Tools, workload.ToolPolicy{Name: name, Mutating: boolp(false)})
	}
	return b
}

// catalogPredicate builds the same predicate compose would hand
// BuildFanout for a bundle: default-deny-unknown plus the bundle's own
// classifications.
func catalogPredicate(b workload.Bundle) effects.Predicate {
	overrides := map[string]bool{}
	for _, p := range b.ToolCatalog.Tools {
		if p.Mutating != nil {
			overrides[p.Name] = *p.Mutating
		}
	}
	return effects.NewPredicate(overrides)
}

func analyst(t *testing.T, name string, tools specialists.ToolAllowlist) Analyst {
	t.Helper()
	return Analyst{Name: name, Agent: buildSpec(t, name, specialists.ModeTask), Tools: tools}
}

func fanoutCfg(t *testing.T, b workload.Bundle, analysts ...Analyst) FanoutConfig {
	t.Helper()
	return FanoutConfig{
		Bundle:    b,
		Analysts:  analysts,
		Synthesis: Specialist{Agent: buildSpec(t, SynthesisName, specialists.ModeTask)},
		Mutating:  catalogPredicate(b),
	}
}

// sawToolResult / blindToTool are what the probe analyst reports as its
// finding title, so a broken branch is a failed assertion with a
// readable message rather than a timeout.
const (
	sawToolResult = "saw-tool-result"
	blindToTool   = "blind-to-tool"
)

// toolProbeModel calls a tool on its first turn and finishes on its
// second, reporting whether the tool's response was in the context it
// was given. That question — can a branch agent see its own tool
// result — is the one the fan-out shape turns on.
type toolProbeModel struct {
	mu    sync.Mutex
	calls int
}

func (m *toolProbeModel) Name() string { return "tool-probe" }

func (m *toolProbeModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	m.calls++
	first := m.calls == 1
	m.mu.Unlock()

	if first {
		return oneResponse(genai.NewPartFromFunctionCall("look", map[string]any{}))
	}
	title := blindToTool
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "look" {
				title = sawToolResult
			}
		}
	}
	return oneResponse(genai.NewPartFromFunctionCall("finish_task", map[string]any{"title": title}))
}

// countingModel finishes immediately and counts how many model calls
// the analysts made, which is how the resume test asserts that an
// approval turn does not re-run them.
type countingModel struct {
	mu sync.Mutex
	n  int
}

func (m *countingModel) Name() string { return "counting" }

func (m *countingModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

func (m *countingModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	m.n++
	m.mu.Unlock()
	return oneResponse(genai.NewPartFromFunctionCall("finish_task", map[string]any{"title": "t"}))
}

// peakModel records the highest number of branches that were inside a
// model call at the same time.
type peakModel struct {
	mu       sync.Mutex
	now, max int
}

func (m *peakModel) Name() string { return "peak" }

func (m *peakModel) peak() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.max
}

func (m *peakModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	m.now++
	if m.now > m.max {
		m.max = m.now
	}
	m.mu.Unlock()
	// Long enough that overlapping branches overlap observably, short
	// enough to stay a unit test. Sleeping needs no CPU, so this holds
	// on a single-core runner too.
	time.Sleep(50 * time.Millisecond)
	m.mu.Lock()
	m.now--
	m.mu.Unlock()
	return oneResponse(genai.NewPartFromFunctionCall("finish_task", map[string]any{"title": "t"}))
}

func oneResponse(parts ...*genai.Part) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:       &genai.Content{Role: genai.RoleModel, Parts: parts},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 1},
			TurnComplete:  true,
			FinishReason:  genai.FinishReasonStop,
		}, nil)
	}
}

// titleSchema is the smallest output schema that makes a specialist
// Task-mode: one required property it reports its verdict in.
var titleSchema = &genai.Schema{
	Type:       genai.TypeObject,
	Properties: map[string]*genai.Schema{"title": {Type: genai.TypeString}},
	Required:   []string{"title"},
}

func toolAnalyst(t *testing.T, name string) adkagent.Agent {
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
	}, specialists.BuildOptions{Model: &toolProbeModel{}, Tools: []tool.Tool{look}})
	if err != nil {
		t.Fatalf("build %q: %v", name, err)
	}
	return a
}

func countingAnalyst(t *testing.T, name string, m model.LLM) adkagent.Agent {
	t.Helper()
	a, err := specialists.Build(specialists.Spec{
		Name: name, Description: name, Mode: specialists.ModeTask,
		Instruction: "report", OutputSchema: titleSchema,
	}, specialists.BuildOptions{Model: m})
	if err != nil {
		t.Fatalf("build %q: %v", name, err)
	}
	return a
}

func TestBuildFanoutRequiresAnalysts(t *testing.T) {
	cfg := fanoutCfg(t, workload.Bundle{Name: "w"})
	if _, err := BuildFanout(cfg); err == nil || !strings.Contains(err.Error(), "no fan-out analysts") {
		t.Fatalf("want no-analysts error, got %v", err)
	}
}

func TestBuildFanoutRequiresSynthesis(t *testing.T) {
	b := catalogBundle("get_k8s_resource")
	cfg := fanoutCfg(t, b, analyst(t, "a", readOnly("get_k8s_resource")))
	cfg.Synthesis = Specialist{}
	if _, err := BuildFanout(cfg); err == nil || !strings.Contains(err.Error(), SynthesisName) {
		t.Fatalf("want synthesis-required error, got %v", err)
	}
}

func TestBuildFanoutAcceptsEnumeratedReadOnlyRoster(t *testing.T) {
	b := catalogBundle("get_k8s_resource", "list_k8s_events")
	root, err := BuildFanout(fanoutCfg(t, b,
		analyst(t, "a", readOnly("get_k8s_resource")),
		analyst(t, "b", readOnly("get_k8s_resource", "list_k8s_events")),
	))
	if err != nil {
		t.Fatalf("BuildFanout: %v", err)
	}
	if root.Name() != "w_fanout" {
		t.Fatalf("root name = %q, want w_fanout", root.Name())
	}
	// The tree is three levels, and the shape is load-bearing: ADK
	// refuses an agent with two parents, so the analysts hang off the
	// fan-out agent and only it and synthesis hang off the root.
	//
	//	w_fanout → w_fan → branch_a → a
	//	                 → branch_b → b
	//	         → _synthesis
	if got := kidNames(root); !reflect.DeepEqual(got, []string{"w_fan", SynthesisName}) {
		t.Fatalf("root sub-agents = %v", got)
	}
	fan := root.SubAgents()[0]
	if got := kidNames(fan); !reflect.DeepEqual(got, []string{"branch_a", "branch_b"}) {
		t.Fatalf("fan sub-agents = %v", got)
	}
	for _, branch := range fan.SubAgents() {
		if got := kidNames(branch); len(got) != 1 || BranchPrefix+got[0] != branch.Name() {
			t.Fatalf("branch %q wraps %v", branch.Name(), got)
		}
	}
}

func kidNames(a adkagent.Agent) []string {
	var out []string
	for _, k := range a.SubAgents() {
		out = append(out, k.Name())
	}
	return out
}

// The three refusals of W3.3. Two of the three are about grants that
// name nothing: under pkg/specialists.filterToolsets an allowlist that
// omits a server (or names one with no tools) is WIDER than one that
// enumerates, so a check that only looked at named tools would pass the
// most dangerous roster of all.
func TestBuildFanoutRefusals(t *testing.T) {
	tests := []struct {
		name    string
		bundle  workload.Bundle
		tools   specialists.ToolAllowlist
		wantErr string
	}{{
		name:    "no allowlist inherits the whole catalog",
		bundle:  catalogBundle("get_k8s_resource"),
		tools:   specialists.ToolAllowlist{},
		wantErr: "declares no tools.mcp allowlist",
	}, {
		name:    "server named with no tools inherits the whole server",
		bundle:  catalogBundle("get_k8s_resource"),
		tools:   specialists.ToolAllowlist{MCP: []specialists.MCPAllowlist{{Server: "gke"}}},
		wantErr: "with no tools: list",
	}, {
		name:    "an explicitly mutating tool",
		bundle:  catalogBundle("get_k8s_resource"),
		tools:   readOnly("get_k8s_resource", "patch_resource"),
		wantErr: "allows mutating tool(s) patch_resource",
	}, {
		name:   "an unclassified tool is mutating by default",
		bundle: catalogBundle(),
		// Nothing about this name says "write". Nothing about it says
		// "read" either, and mast believes the second one.
		tools:   readOnly("get_k8s_resource"),
		wantErr: "allows mutating tool(s) get_k8s_resource",
	}, {
		name:   "a spawning builtin is not read-only",
		bundle: catalogBundle("get_k8s_resource"),
		tools: specialists.ToolAllowlist{
			Builtin: []string{"invoke_specialist"},
			MCP:     []specialists.MCPAllowlist{{Server: "gke", Tools: []string{"get_k8s_resource"}}},
		},
		wantErr: "allows mutating built-in tool(s) invoke_specialist",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildFanout(fanoutCfg(t, tc.bundle, analyst(t, "a", tc.tools)))
			if err == nil {
				t.Fatalf("want refusal containing %q, got a built root", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// A workload with no MCP catalog has no toolset for an empty allowlist
// to inherit, so the "enumerate your tools" rule has nothing to bite on
// and must not fire. Without this the refusal would reject every
// tool-free roster, including the fixtures the rest of these tests use.
func TestBuildFanoutNoCatalogNeedsNoAllowlist(t *testing.T) {
	if _, err := BuildFanout(fanoutCfg(t, workload.Bundle{Name: "w"}, analyst(t, "a", specialists.ToolAllowlist{}))); err != nil {
		t.Fatalf("BuildFanout with no tool catalog: %v", err)
	}
}

// The refusal has to hold for the SHIPPED mutating roster, which is the
// case an operator actually reaches by typing --dispatch=fanout at a
// triage bundle. gke-triage's diagnosers carry patch_resource and
// rollout_undo; the UAT's U-fanout-refuse leg drives the same thing
// through the binary.
func TestBuildFanoutRefusesRemediationRoster(t *testing.T) {
	b := catalogBundle("get_k8s_resource")
	b.ToolCatalog.Tools = append(b.ToolCatalog.Tools, workload.ToolPolicy{Name: "patch_resource", Mutating: boolp(true)})
	_, err := BuildFanout(fanoutCfg(t, b,
		analyst(t, "OOMKilled", readOnly("get_k8s_resource", "patch_resource")),
	))
	if err == nil || !strings.Contains(err.Error(), "patch_resource") {
		t.Fatalf("want a refusal naming patch_resource, got %v", err)
	}
}

func TestMaxConcurrency(t *testing.T) {
	if got := maxConcurrency(workload.Bundle{}); got != DefaultMaxConcurrency {
		t.Fatalf("unset = %d, want %d", got, DefaultMaxConcurrency)
	}
	if got := maxConcurrency(workload.Bundle{Fanout: workload.Fanout{MaxConcurrency: 2}}); got != 2 {
		t.Fatalf("explicit = %d, want 2", got)
	}
	// Negative is ADK's own spelling of "unbounded" and is passed
	// through rather than clamped to the default.
	if got := maxConcurrency(workload.Bundle{Fanout: workload.Fanout{MaxConcurrency: -1}}); got != -1 {
		t.Fatalf("negative = %d, want -1 (unbounded)", got)
	}
}

// findingsFor is the whole of W3.2's contract in one function: the
// roster decides the order, an analyst that reported nothing is Silent,
// and there is no other source of truth to fall back on.
func TestFindingsFor(t *testing.T) {
	order := []string{"a", "b", "c"}

	t.Run("roster order, not map order", func(t *testing.T) {
		f := findingsFor(order, map[string]any{"c": "fc", "a": "fa", "b": "fb"})
		if len(f.Silent) != 0 {
			t.Fatalf("Silent = %v, want none", f.Silent)
		}
		for i, want := range []string{"fa", "fb", "fc"} {
			if f.Reported[i].Analyst != order[i] || f.Reported[i].Payload != want {
				t.Fatalf("Reported[%d] = %+v, want {%s %s}", i, f.Reported[i], order[i], want)
			}
		}
	})

	t.Run("a missing analyst is silence, and it is the right branch", func(t *testing.T) {
		f := findingsFor(order, map[string]any{"a": "fa", "c": "fc"})
		if len(f.Reported) != 2 || f.Reported[1].Analyst != "c" {
			t.Fatalf("Reported = %+v, want a and c", f.Reported)
		}
		if len(f.Silent) != 1 || f.Silent[0] != "b" {
			t.Fatalf("Silent = %v, want [b]", f.Silent)
		}
	})

	t.Run("an absent or nil-valued map is all-silent, not a panic", func(t *testing.T) {
		if got := findingsFor(order, nil); len(got.Silent) != 3 {
			t.Fatalf("nil map = %+v, want all silent", got)
		}
		if got := findingsFor(order, map[string]any{"a": nil}); len(got.Silent) != 3 {
			t.Fatalf("nil payload = %+v, want all silent", got)
		}
	})
}

func TestSynthesisPromptShowsSilenceAndNothingElse(t *testing.T) {
	got := SynthesisPrompt(&Findings{
		Reported: []Finding{{Analyst: "a", Payload: "payload-a"}},
		Silent:   []string{"b", "c"},
	})
	for _, want := range []string{"## a", "payload-a", "Returned no finding: b, c"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}

// An all-silent fan-out reaches no model and raises no gate. The graph
// version of this was a W1.4 finding — an operator asked to approve
// `Result: <nil>` — and the short-circuit is what keeps fan-out from
// reproducing it. Passing a nil context and a nil node is the
// assertion, not a shortcut: if synthesize ever grew a model call or a
// gate on this path it would have to touch one of them.
func TestSynthesizeAllSilentSkipsTheGate(t *testing.T) {
	b := workload.Bundle{Name: "w", HITL: workload.HITL{RequireApproval: true}}
	out, err := synthesize(nil, b, nil, &Findings{Silent: []string{"a", "b"}}, nil)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result = %T, want map", out)
	}
	if m["report"] != "" {
		t.Fatalf("report = %v, want empty", m["report"])
	}
	if m["reported"] != 0 || m["analysts"] != 2 {
		t.Fatalf("counts = %v/%v, want 0/2", m["reported"], m["analysts"])
	}
}

// TestFanoutRunMergesFindings drives the assembled shape end to end
// over the offline echo model: every analyst runs, every payload
// reaches synthesis attributed to its branch, and the run parks on the
// one gate.
func TestFanoutRunMergesFindings(t *testing.T) {
	b := catalogBundle("get_k8s_resource")
	b.HITL.RequireApproval = true
	b.Fanout.MaxConcurrency = 2
	root, err := BuildFanout(fanoutCfg(t, b,
		analyst(t, "alpha", readOnly("get_k8s_resource")),
		analyst(t, "beta", readOnly("get_k8s_resource")),
		analyst(t, "gamma", readOnly("get_k8s_resource")),
	))
	if err != nil {
		t.Fatalf("BuildFanout: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           "fanout-test",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
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
		t.Fatal("fan-out run finished without raising the synthesis gate")
	}
	if pending.InterruptID != SynthesisInterruptID {
		t.Fatalf("interrupt = %q, want %q", pending.InterruptID, SynthesisInterruptID)
	}
	// Three analysts, all reporting: the count in the operator's prompt
	// is the count of branches that returned a payload, so "3 of 3" is
	// the assertion that no branch was dropped or double-counted.
	if !strings.Contains(pending.Message, "3 of 3 analysts") {
		t.Fatalf("gate message = %q, want it to report 3 of 3 analysts", pending.Message)
	}
}

// TestBranchAgentSeesItsToolResults is why this shape is built on
// parallelagent instead of workflow.ParallelWorker, and it is the
// regression test for the whole rewrite.
//
// An LLM agent's working memory is the session event list, and only
// events that reach the runner get appended to it. ParallelWorker
// suppresses every branch event that is not an output event, so an
// analyst's second model call sees the same prompt as its first: no
// tool call, no tool result. A tool-using analyst then loops until
// something cancels it, which is what an analyst that reads a cluster
// does on every run.
//
// The probe model is bounded (it gives up after the second call) so
// that a regression FAILS here rather than hanging — a neutralize run
// that hangs teaches nothing.
func TestBranchAgentSeesItsToolResults(t *testing.T) {
	b := catalogBundle("get_k8s_resource")
	root, err := BuildFanout(FanoutConfig{
		Bundle:    b,
		Analysts:  []Analyst{{Name: "alpha", Agent: toolAnalyst(t, "alpha"), Tools: readOnly("get_k8s_resource")}},
		Synthesis: Specialist{Agent: buildSpec(t, SynthesisName, specialists.ModeTask)},
		Mutating:  catalogPredicate(b),
	})
	if err != nil {
		t.Fatalf("BuildFanout: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "fanout-tools", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	var saw any
	for ev, err := range r.Run(context.Background(), "op", "s1", genai.NewContentFromText("audit prod", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev.Author == "alpha" && ev.Output != nil {
			saw = ev.Output
		}
	}
	if saw == nil {
		t.Fatal("no finding from the analyst reached the runner (a branch that yields nothing upward cannot be metered, logged, or merged)")
	}
	payload, _ := saw.(map[string]any)
	if payload["title"] != sawToolResult {
		t.Fatalf("analyst reported %v, want title=%q — its second model call could not see the result of its own tool call", saw, sawToolResult)
	}
}

// TestFanoutResumeKeepsTheFindings: the approval turn must not re-run
// the analysts, and the merged report the operator approved must be the
// one that comes back.
func TestFanoutResumeKeepsTheFindings(t *testing.T) {
	b := catalogBundle("get_k8s_resource")
	b.HITL.RequireApproval = true
	counting := &countingModel{}
	root, err := BuildFanout(FanoutConfig{
		Bundle: b,
		Analysts: []Analyst{
			{Name: "alpha", Agent: countingAnalyst(t, "alpha", counting), Tools: readOnly("get_k8s_resource")},
			{Name: "beta", Agent: countingAnalyst(t, "beta", counting), Tools: readOnly("get_k8s_resource")},
		},
		Synthesis: Specialist{Agent: buildSpec(t, SynthesisName, specialists.ModeTask)},
		Mutating:  catalogPredicate(b),
	})
	if err != nil {
		t.Fatalf("BuildFanout: %v", err)
	}
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName: "fanout-resume", Agent: root,
		SessionService: svc, AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	for ev, err := range r.Run(context.Background(), "op", "s1", genai.NewContentFromText("audit prod", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		_ = ev
	}
	before := counting.calls()
	if before == 0 {
		t.Fatal("no analyst ran on the first turn")
	}

	// The resume message shape cmd/mast.resume builds: an
	// adk_request_input FunctionResponse whose ID is the interrupt ID.
	msg := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromFunctionResponse("adk_request_input", map[string]any{
			"response": map[string]any{"approved": true, "note": "ok"},
		})},
	}
	msg.Parts[0].FunctionResponse.ID = SynthesisInterruptID

	var final map[string]any
	for ev, err := range r.Run(context.Background(), "op", "s1", msg, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if m, ok := ev.Output.(map[string]any); ok && m["analysts"] != nil {
			final = m
		}
	}
	if final == nil {
		t.Fatal("resume produced no fan-out result")
	}
	if got := counting.calls(); got != before {
		t.Fatalf("analyst model calls went %d → %d across the resume; the approval turn must not re-run the branches", before, got)
	}
	if final["approval"] == nil {
		t.Fatalf("resume result carries no approval: %v", final)
	}
	if final["reported"] != 2 || final["analysts"] != 2 {
		t.Fatalf("resume result counts = %v of %v, want 2 of 2 — the findings did not survive the pause", final["reported"], final["analysts"])
	}
}

// TestFanoutRespectsMaxConcurrency: parallelagent starts every
// sub-agent at once, so the cap is mast's to enforce.
func TestFanoutRespectsMaxConcurrency(t *testing.T) {
	run := func(t *testing.T, limit int) int {
		t.Helper()
		b := catalogBundle("get_k8s_resource")
		b.Fanout.MaxConcurrency = limit
		peak := &peakModel{}
		var analysts []Analyst
		for _, name := range []string{"a", "b", "c", "d"} {
			analysts = append(analysts, Analyst{
				Name: name, Agent: countingAnalyst(t, name, peak), Tools: readOnly("get_k8s_resource"),
			})
		}
		root, err := BuildFanout(FanoutConfig{
			Bundle: b, Analysts: analysts,
			Synthesis: Specialist{Agent: buildSpec(t, SynthesisName, specialists.ModeTask)},
			Mutating:  catalogPredicate(b),
		})
		if err != nil {
			t.Fatalf("BuildFanout: %v", err)
		}
		r, err := runner.New(runner.Config{
			AppName: "fanout-conc", Agent: root,
			SessionService: session.InMemoryService(), AutoCreateSession: true,
		})
		if err != nil {
			t.Fatalf("runner.New: %v", err)
		}
		for _, err := range r.Run(context.Background(), "op", "s1", genai.NewContentFromText("audit", genai.RoleUser), adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("run: %v", err)
			}
		}
		return peak.peak()
	}

	// Unbounded first: if branches did not actually overlap, the capped
	// assertion below would pass for the wrong reason.
	if got := run(t, -1); got < 2 {
		t.Fatalf("unbounded peak concurrency = %d, want at least 2 (the branches did not overlap at all)", got)
	}
	if got := run(t, 2); got > 2 {
		t.Fatalf("peak concurrency = %d with max_concurrency 2", got)
	}
}
