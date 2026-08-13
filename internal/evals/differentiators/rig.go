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

package differentiators

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/genai"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/mast/internal/compose"
	"github.com/go-steer/mast/internal/evals"
	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/effects"
	"github.com/go-steer/mast/pkg/permissions"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// The rig runs against a real SQLite session store, the same ADK
// database service cmd/mast and the library root use. Scenarios about
// interrupted turns and replayed effects are exactly the ones the
// in-memory service cannot express.
const (
	appName  = "mast"
	userID   = "mast-evals"
	rigModel = "differentiator-script"
)

// Tool names. The read side is a real lookout intent from the parity
// corpus (testdata/evals/intents.yaml), so a differentiator's trace
// scores through the same intent table the 31-row corpus does. The
// write side is the change-executor shape W2.4 splits the roster on.
const (
	toolTriage  = "k8s_triage_workload"
	toolScale   = "scale_deployment"
	toolRestart = "rollout_restart"
)

// specialistName is the roster's one Task specialist. It must not
// collide with a mutating tool name — effects.CheckNameCollisions
// refuses that at construction on every real path, and the rig runs
// the same check so a fixture cannot drift into the fail-open hole.
const specialistName = "remediator"

// operator is the human in the loop, written against the interface
// mast already has for asking one: permissions.Prompter.
//
// Using the real interface rather than a fixture-local callback is the
// point. The approval scenarios do not claim mast has no way to ask an
// operator; they claim nothing on the tool-execution path reaches the
// way it has. An operator that implements Prompter and is never called
// says exactly that, and when W2.1 wires the gate this type needs no
// change to start being consulted.
//
// Note also what the interface cannot say. AskApproval answers with a
// permissions.Decision — deny, allow-once, allow-session, and so on —
// and no member of that set means "allow, but with these arguments".
// E-approval-edited's operator therefore carries its edit in a field
// alongside the Decision, readable only by the fixture. That is the
// W2.5 gap stated in the type system rather than only in prose.
type operator struct {
	// decide answers one request. The second return is the operator's
	// edited arguments, which have no channel in Decision.
	decide func(req permissions.PromptRequest) (permissions.Decision, map[string]any)

	mu       sync.Mutex
	consults int
}

var _ permissions.Prompter = (*operator)(nil)

func (o *operator) AskApproval(_ context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
	o.mu.Lock()
	o.consults++
	o.mu.Unlock()
	d, _ := o.decide(req)
	return d, nil
}

func (o *operator) consulted() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.consults
}

// rigConfig is what a scenario declares about its fixture.
type rigConfig struct {
	dir string

	// specs is the roster. Empty means the default one-Task-specialist
	// roster; the budget scenario supplies its own so it can declare a
	// per-specialist ceiling.
	specs []specialists.Spec

	// limits are the workload's budget ceilings, composed the way
	// mast.limits composes them (bundle block over the model rate).
	limits budget.Limits

	// tokensPerCall is the scripted model's reported usage. Explicit
	// because the budget scenario's arithmetic depends on it.
	tokensPerCall int32

	// steps drive the scripted model; see scriptModel.
	steps []step

	// op is the operator the fixture offers. Nil means no approval is
	// expected.
	//
	// Nothing wires it into the runtime, because no mast construction
	// path accepts one: that is the gap E-approval-rejected and
	// E-approval-edited measure. When W2.1 lands, this is the field
	// that gets connected, and the two scenarios flip without their
	// fixtures changing.
	op *operator
}

// rig is one composed mast runtime plus the counters a scenario checks.
type rig struct {
	cfg    rigConfig
	svc    adksession.Service
	store  *transcript.Store
	runner *runner.Runner
	model  *scriptModel
	pred   effects.Predicate
	subs   map[string]bool

	mu sync.Mutex
	// mutations records the arguments of every scale_deployment and
	// rollout_restart execution that actually reached the tool body.
	// Recording the args, not just a count, is what lets
	// E-approval-edited ask whose arguments ran.
	mutations []executedCall
	reads     int
}

type executedCall struct {
	Name string
	Args map[string]any
}

func newRig(ctx context.Context, cfg rigConfig) (*rig, error) {
	if cfg.dir == "" {
		return nil, fmt.Errorf("rig: no scratch dir")
	}
	if cfg.tokensPerCall == 0 {
		cfg.tokensPerCall = 15
	}
	r := &rig{cfg: cfg, model: &scriptModel{steps: cfg.steps, tokens: cfg.tokensPerCall}}

	// Silence GORM the way pkg/eventlog.Open does. Left at ADK's default
	// the five scenarios emit ~48KB of SELECT and "record not found"
	// chatter, which buries the report this suite exists to print.
	svc, err := database.NewSessionService(
		sqlite.Open(filepath.Join(cfg.dir, "sessions.db")),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)},
	)
	if err != nil {
		return nil, fmt.Errorf("rig: open session store: %w", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		return nil, fmt.Errorf("rig: migrate session store: %w", err)
	}
	r.svc = svc
	r.store = transcript.NewStore(svc, appName)

	tools, err := r.buildTools()
	if err != nil {
		return nil, err
	}

	specs := cfg.specs
	if len(specs) == 0 {
		specs = []specialists.Spec{{
			Name:        specialistName,
			Description: "remediates a workload",
			Mode:        specialists.ModeTask,
			Instruction: "Triage the incident and remediate it.",
		}}
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	bundle := workload.Bundle{
		Name:        "differentiators",
		Description: "v0.3 differentiator eval fixture",
		Specialists: names,
		// The read tool is un-gated through the audited override, the
		// same mechanism a real bundle uses: unknown tools default to
		// mutating, and ADK's mcptoolset drops MCP annotations, so the
		// catalog is the only place a read tool can be declared safe.
		ToolCatalog: workload.ToolCatalog{Tools: []workload.ToolPolicy{
			{Name: toolTriage, Mutating: boolPtr(false)},
		}},
		Budget: workload.Budget{
			MaxCostUSD: cfg.limits.MaxCostUSD,
			MaxTurns:   cfg.limits.MaxTurns,
		},
	}

	root, err := compose.BuildRoot(ctx, compose.RootConfig{
		Bundle:    bundle,
		Specs:     specs,
		Model:     r.model,
		ModelName: rigModel,
		Toolsets:  []tool.Toolset{&staticToolset{name: "differentiators", tools: tools}},
	})
	if err != nil {
		return nil, fmt.Errorf("rig: compose root: %w", err)
	}

	var policies []effects.ToolPolicy
	for _, p := range bundle.ToolCatalog.Tools {
		policies = append(policies, effects.ToolPolicy{Name: p.Name, Mutating: p.Mutating})
	}
	r.pred = effects.NewPredicate(effects.Overrides(nil, policies))
	r.subs = effects.SubAgentNames(root)
	if hits := effects.CheckNameCollisions(r.subs, r.pred, policies); len(hits) > 0 {
		return nil, fmt.Errorf("rig: fixture names both a sub-agent and a mutating tool: %s", strings.Join(hits, ", "))
	}
	outbox, err := effects.New(effects.Config{
		Predicate:     r.pred,
		SubAgentNames: r.subs,
		AckedAt: func(ctx context.Context, sid string) (time.Time, bool) {
			return r.store.EffectsAckedAt(ctx, "", sid)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("rig: construct effects outbox: %w", err)
	}
	r.runner, err = runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{outbox}},
	})
	if err != nil {
		return nil, fmt.Errorf("rig: construct runner: %w", err)
	}
	return r, nil
}

func (r *rig) buildTools() ([]tool.Tool, error) {
	triage, err := functiontool.New(functiontool.Config{
		Name:        toolTriage,
		Description: "Read-only workload triage snapshot.",
	}, func(_ adkagent.Context, args triageArgs) (map[string]any, error) {
		r.mu.Lock()
		r.reads++
		r.mu.Unlock()
		return map[string]any{
			"workload": args.Workload,
			"severity": "CRITICAL",
			"finding":  "container OOMKilled 4 times in 10m; memory limit 128Mi",
		}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("rig: build %s: %w", toolTriage, err)
	}
	scale, err := functiontool.New(functiontool.Config{
		Name:        toolScale,
		Description: "Scale a deployment. Mutating.",
	}, func(_ adkagent.Context, args scaleArgs) (map[string]any, error) {
		r.record(toolScale, map[string]any{"deployment": args.Deployment, "replicas": args.Replicas})
		return map[string]any{"deployment": args.Deployment, "replicas": args.Replicas}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("rig: build %s: %w", toolScale, err)
	}
	restart, err := functiontool.New(functiontool.Config{
		Name:        toolRestart,
		Description: "Restart a deployment's pods. Mutating.",
	}, func(_ adkagent.Context, args restartArgs) (map[string]any, error) {
		r.record(toolRestart, map[string]any{"deployment": args.Deployment})
		return map[string]any{"restarted": args.Deployment}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("rig: build %s: %w", toolRestart, err)
	}
	return []tool.Tool{triage, scale, restart}, nil
}

type triageArgs struct {
	Workload string `json:"workload"`
}

type scaleArgs struct {
	Deployment string `json:"deployment"`
	Replicas   int    `json:"replicas"`
}

type restartArgs struct {
	Deployment string `json:"deployment"`
}

func (r *rig) record(name string, args map[string]any) {
	r.mu.Lock()
	r.mutations = append(r.mutations, executedCall{Name: name, Args: args})
	r.mu.Unlock()
}

// executed returns the mutations that actually ran, in order.
func (r *rig) executed() []executedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]executedCall, len(r.mutations))
	copy(out, r.mutations)
	return out
}

func (r *rig) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// turn drives one user turn through the runner, metering it against
// the workload limits exactly as mast.runTurn does: the meter folds
// every streamed event and a crossed ceiling cancels the run.
//
// A budget stop is not an error here — it is an outcome scenarios ask
// about — so it is returned separately from the transport errors that
// mean the fixture is broken.
func (r *rig) turn(ctx context.Context, sessionID, text string) (stopped error, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.model.beginTurn()
	meter := budget.NewMeter(r.cfg.limits)
	msg := genai.NewContentFromText(text, genai.RoleUser)
	for ev, rerr := range r.runner.Run(ctx, userID, sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if rerr != nil {
			return stopped, fmt.Errorf("rig: run turn: %w", rerr)
		}
		if berr := meter.Observe(ev); berr != nil {
			cancel()
			return berr, nil
		}
	}
	return nil, nil
}

// roleCalls reports how many model calls one agent in the composed
// shape has made. Per-role counting is what lets the budget scenario
// ask about the specialist's own spend rather than the session's.
func (r *rig) roleCalls(who role) int { return r.model.roleCalls(who) }

// operatorConsults reports how many times the fixture's operator was
// asked to approve something. Zero on every path today.
func (r *rig) operatorConsults() int {
	if r.cfg.op == nil {
		return 0
	}
	return r.cfg.op.consulted()
}

// trace reads the session back and scores it through the same adapter
// the 31-row corpus uses.
func (r *rig) trace(ctx context.Context, sessionID string) (evals.Trace, error) {
	resp, err := r.svc.Get(ctx, &adksession.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		return evals.Trace{}, fmt.Errorf("rig: read session %q: %w", sessionID, err)
	}
	return evals.TraceFromEvents(resp.Session.Events(), r.pred, r.subs), nil
}

// seedDangling appends an unpaired mutating FunctionCall — the wire
// shape a SIGKILL mid-tool-execution leaves behind — to a fresh
// session, and returns once its timestamp is safely in the past.
func (r *rig) seedDangling(ctx context.Context, sessionID, toolName, callID string) error {
	created, err := r.svc.Create(ctx, &adksession.CreateRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		return fmt.Errorf("rig: create session %q: %w", sessionID, err)
	}
	ev := adksession.NewEvent(ctx, "interrupted-invocation")
	ev.Author = specialistName
	part := genai.NewPartFromFunctionCall(toolName, map[string]any{"deployment": "api", "replicas": 3})
	part.FunctionCall.ID = callID
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}}
	if err := r.svc.AppendEvent(ctx, created.Session, ev); err != nil {
		return fmt.Errorf("rig: seed dangling intent: %w", err)
	}
	// Natural forward timestamps for everything that follows — a
	// fixture whose clock runs backwards disarms the ack watermark
	// comparison this scenario depends on (mast #54).
	time.Sleep(5 * time.Millisecond)
	return nil
}

// toolResponse returns the recorded FunctionResponse payloads for a
// tool name across the whole session.
func (r *rig) toolResponses(ctx context.Context, sessionID, toolName string) ([]map[string]any, error) {
	resp, err := r.svc.Get(ctx, &adksession.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("rig: read session %q: %w", sessionID, err)
	}
	var out []map[string]any
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == toolName {
				out = append(out, p.FunctionResponse.Response)
			}
		}
	}
	return out, nil
}

// staticToolset offers a fixed tool list, the minimum tool.Toolset a
// fixture needs to reach specialists.Build's allowlist filter.
type staticToolset struct {
	name  string
	tools []tool.Tool
}

func (s *staticToolset) Name() string { return s.name }

func (s *staticToolset) Tools(adkagent.ReadonlyContext) ([]tool.Tool, error) { return s.tools, nil }

// role distinguishes the two agents in the composed shape. The
// coordinator and the Task specialist share one model instance, so the
// script matches on which of them is asking rather than on a global
// call counter — a sequencing surprise in the dispatch shape then
// leaves the fixture Broken instead of silently answering the wrong
// agent.
type role int

const (
	anyRole role = iota
	coordinatorRole
	specialistRole
)

// step is one scripted model reply, addressed to a role within a turn.
// turn is 1-based and every helper defaults it to the first turn; a
// multi-turn scenario relabels its later replies with onTurn.
type step struct {
	turn int
	role role
	resp *model.LLMResponse
}

func specialistStep(resp *model.LLMResponse) step {
	return step{turn: 1, role: specialistRole, resp: resp}
}

// onTurn readdresses a step to the nth turn (1-based).
//
// Turn addressing is not cosmetic. Selecting purely on role lets an
// agent that runs out of replies in turn 1 reach forward and consume
// turn 2's — which is how the first draft of E-exactly-once fired both
// of its scale calls inside a single invocation and "observed" a
// double effect the runtime never had a chance to deduplicate (the
// outbox snapshots history once per turn, in BeforeRun). A fixture
// that quietly collapses two turns into one is not evidence about
// interrupt/resume.
func onTurn(n int, s step) step {
	s.turn = n
	return s
}

// scriptModel answers each model call with the next unused step
// addressed to the current turn and the calling agent's role, and falls
// back to finishing the turn.
type scriptModel struct {
	steps  []step
	tokens int32

	mu     sync.Mutex
	turnNo int
	used   []bool
	calls  int
	byRole map[role]int
}

func (m *scriptModel) Name() string { return rigModel }

func (m *scriptModel) roleCalls(who role) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byRole[who]
}

// beginTurn advances the script to the next turn's replies. The rig
// calls it once per runner.Run.
func (m *scriptModel) beginTurn() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turnNo++
}

func (m *scriptModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		who := roleOf(req)
		m.mu.Lock()
		m.calls++
		if m.byRole == nil {
			m.byRole = map[role]int{}
		}
		m.byRole[who]++
		if m.used == nil {
			m.used = make([]bool, len(m.steps))
		}
		var resp *model.LLMResponse
		for i, s := range m.steps {
			if m.used[i] || s.turn != m.turnNo {
				continue
			}
			if s.role != anyRole && s.role != who {
				continue
			}
			m.used[i] = true
			resp = s.resp
			break
		}
		m.mu.Unlock()

		if resp == nil {
			resp = defaultReply(who)
		}
		out := *resp
		out.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     m.tokens,
			CandidatesTokenCount: 0,
			TotalTokenCount:      m.tokens,
		}
		out.TurnComplete = true
		out.FinishReason = genai.FinishReasonStop
		yield(&out, nil)
	}
}

// defaultReply keeps a turn terminating once the script runs out: the
// specialist finishes its task, the coordinator says its piece.
func defaultReply(who role) *model.LLMResponse {
	if who == specialistRole {
		return callTo("finish_task", map[string]any{"result": "remediation complete"})
	}
	return textOf("CRITICAL: workload api is OOMKilling; remediation reported above.")
}

// roleOf reads the calling agent's role off the tools it was offered.
// finish_task is the Task specialist's completion tool and nothing else
// carries it; the coordinator's own roster is its sub-agents.
func roleOf(req *model.LLMRequest) role {
	if req == nil || req.Config == nil {
		return anyRole
	}
	for _, t := range req.Config.Tools {
		if t == nil {
			continue
		}
		for _, d := range t.FunctionDeclarations {
			if d != nil && d.Name == "finish_task" {
				return specialistRole
			}
		}
	}
	return coordinatorRole
}

func callTo(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
	}}
}

// callToWithID pins the function-call ID, which the interrupt/resume
// scenario needs: the outbox's exact-key replay is keyed on it.
func callToWithID(name, id string, args map[string]any) *model.LLMResponse {
	resp := callTo(name, args)
	resp.Content.Parts[0].FunctionCall.ID = id
	return resp
}

func textOf(s string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromText(s, genai.RoleModel)}
}

func boolPtr(b bool) *bool { return &b }

// scratch makes a per-scenario subdirectory under the caller's root.
func scratch(root, id string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("differentiators: no scratch root (house rule #5: derive it from os.TempDir)")
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("differentiators: scratch dir for %q: %w", id, err)
	}
	return dir, nil
}
