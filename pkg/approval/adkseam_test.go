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
	"iter"
	"path/filepath"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"google.golang.org/genai"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

// The four facts this file pins, all undocumented by ADK and all
// load-bearing for the write gate:
//
//  1. A PLUGIN can drive the confirmation flow. Every ADK example puts
//     RequestConfirmation inside the tool body, which would mean a gate
//     mast can only apply to tools mast wrote — useless for MCP verbs,
//     which are the calls that change a cluster. The plugin callback
//     receives the same agent.Context the tool would, so it can request
//     the confirmation and short-circuit the call.
//  2. The pause is durable and the tool does not run.
//  3. On approval the SAME call re-fires, exactly once, and the callback
//     sees the operator's verdict through ctx.ToolConfirmation().
//  4. The operator's payload round-trips, and mutating the args map in
//     the callback changes what the tool executes. This is the only
//     channel an EDITED verdict (W2.5) can travel through: ADK re-fires
//     the original call verbatim from the recorded confirmation request,
//     so an edit has to be applied on the way past.

const (
	testApp  = "approval-seam"
	testUser = "operator"
	sid      = "seam-session"
)

func sqliteService(t *testing.T) adksession.Service {
	t.Helper()
	return sqliteServiceAt(t, t.TempDir())
}

// sqliteServiceAt opens a session store in an explicit directory, so a
// test can open the SAME store twice and stand in for a restart.
func sqliteServiceAt(t *testing.T, dir string) adksession.Service {
	t.Helper()
	// Silent logger: ADK's default GORM logger writes every statement to
	// stderr, which buries a probe's own output (a W0.4 finding — the
	// quiet logger is mast's, in pkg/eventlog, not ADK's).
	quiet := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	svc, err := database.NewSessionService(sqlite.Open(filepath.Join(dir, "sessions.db")), quiet)
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return svc
}

// scriptedModel replies from a fixed script, one response per call.
type scriptedModel struct {
	name  string
	mu    sync.Mutex
	round int
	calls []*model.LLMResponse
}

func (m *scriptedModel) Name() string { return m.name }

func (m *scriptedModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		round := m.round
		m.round++
		m.mu.Unlock()
		var resp *model.LLMResponse
		if round < len(m.calls) {
			resp = m.calls[round]
		} else {
			resp = &model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel)}
		}
		resp.TurnComplete = true
		resp.FinishReason = genai.FinishReasonStop
		yield(resp, nil)
	}
}

type scaleArgs struct {
	Deployment string `json:"deployment"`
	Replicas   int    `json:"replicas"`
}

// seamProbe runs one mutating call through a plugin-driven confirmation
// and reports what the tool saw. gate is the plugin's BeforeToolCallback
// under test; respond builds the operator's turn-2 answer from the
// confirmation call's ID (nil = no second turn, leaving the run parked).
type seamProbe struct {
	// executions is one entry per actual tool execution, holding the
	// arguments it ran with.
	executions []scaleArgs
	// confirmationID is the adk_request_confirmation call the flow
	// emitted, empty if it never paused.
	confirmationID string
	// hintSeen is the hint recorded in the pause request.
	hintSeen string
	// verdicts is one entry per callback invocation: nil before the
	// operator answers, the confirmation afterwards.
	verdicts []*toolconfirmation.ToolConfirmation
}

func runSeamProbe(t *testing.T, gate func(p *seamProbe) llmagent.BeforeToolCallback, respond func(confID string) *genai.Content) *seamProbe {
	t.Helper()
	probe := &seamProbe{}

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

	m := &scriptedModel{name: "seam", calls: []*model.LLMResponse{{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", map[string]any{
				"deployment": "api",
				"replicas":   10,
			})},
		},
	}}}

	root, err := llmagent.New(llmagent.Config{
		Name:        "seam_agent",
		Description: "write-gate seam probe",
		Instruction: "act",
		Model:       m,
		Tools:       []tool.Tool{scale},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	p, err := plugin.New(plugin.Config{
		Name:               "seam-gate",
		BeforeToolCallback: gate(probe),
	})
	if err != nil {
		t.Fatalf("plugin.New: %v", err)
	}

	svc := sqliteService(t)
	r, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{p}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	run := func(msg *genai.Content) {
		t.Helper()
		for _, err := range r.Run(context.Background(), testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("runner.Run: %v", err)
			}
		}
	}

	run(genai.NewContentFromText("scale api to 10", genai.RoleUser))
	probe.confirmationID, probe.hintSeen = confirmationRequest(t, svc)

	if respond != nil && probe.confirmationID != "" {
		run(respond(probe.confirmationID))
	}
	return probe
}

// confirmationRequest finds the adk_request_confirmation call the flow
// parked on, and the hint the gate attached to it.
func confirmationRequest(t *testing.T, svc adksession.Service) (id, hint string) {
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
			if part == nil || part.FunctionCall == nil {
				continue
			}
			if part.FunctionCall.Name != toolconfirmation.FunctionCallName {
				continue
			}
			id = part.FunctionCall.ID
			// The request's args survive the event log as JSON, so the
			// toolConfirmation ADK put there as a typed value reads back
			// as a map. Both shapes are handled: in-process consumers
			// (an attach client tailing live events) see the struct, and
			// anything reading the durable log sees the map.
			switch tc := part.FunctionCall.Args["toolConfirmation"].(type) {
			case toolconfirmation.ToolConfirmation:
				hint = tc.Hint
			case map[string]any:
				hint, _ = tc["hint"].(string)
			}
		}
	}
	return id, hint
}

// verdictResponse builds the operator's turn: a FunctionResponse under
// the confirmation call's ID. This is the wire shape pkg/inject's
// /resume will have to produce, so the probe writes it by hand rather
// than through a helper that could hide a field.
func verdictResponse(confID string, response map[string]any) *genai.Content {
	fr := genai.NewPartFromFunctionResponse(toolconfirmation.FunctionCallName, response)
	fr.FunctionResponse.ID = confID
	return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{fr}}
}

// askOnce is the minimal gate: ask the first time, honor the verdict the
// second, applying any edited arguments the operator sent back.
func askOnce(p *seamProbe) llmagent.BeforeToolCallback {
	return func(ctx adkagent.Context, tl tool.Tool, args map[string]any) (map[string]any, error) {
		c := ctx.ToolConfirmation()
		p.verdicts = append(p.verdicts, c)
		if c == nil {
			if err := ctx.RequestConfirmation("approve "+tl.Name()+"?", map[string]any{"args": args}); err != nil {
				return nil, err
			}
			// A non-nil response short-circuits the call: ADK names the
			// return value newArgs, but callTool assigns it to the tool's
			// RESPONSE. Returning nil here would run the tool.
			return map[string]any{"status": "awaiting operator approval"}, nil
		}
		if !c.Confirmed {
			return map[string]any{"status": "rejected by operator"}, nil
		}
		if payload, ok := c.Payload.(map[string]any); ok {
			if edited, ok := payload["args"].(map[string]any); ok {
				for k := range args {
					delete(args, k)
				}
				for k, v := range edited {
					args[k] = v
				}
			}
		}
		return nil, nil
	}
}

// TestSeamPausesBeforeTheToolRuns pins facts 1 and 2: a plugin can drive
// the confirmation flow, and the tool does not run while the operator
// has not answered.
func TestSeamPausesBeforeTheToolRuns(t *testing.T) {
	p := runSeamProbe(t, askOnce, nil)

	if p.confirmationID == "" {
		t.Fatalf("no %s call emitted — a plugin cannot drive the confirmation flow, and the whole gate design rests on it", toolconfirmation.FunctionCallName)
	}
	if p.hintSeen != "approve scale_deployment?" {
		t.Errorf("hint = %q, want the gate's own text — the operator prompt is what mast writes here", p.hintSeen)
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) while parked, want 0: %+v", len(p.executions), p.executions)
	}
	if len(p.verdicts) != 1 || p.verdicts[0] != nil {
		t.Errorf("callback verdicts = %+v, want exactly one pre-verdict (nil) call", p.verdicts)
	}
}

// TestSeamApprovalRunsTheToolExactlyOnce pins fact 3.
func TestSeamApprovalRunsTheToolExactlyOnce(t *testing.T) {
	p := runSeamProbe(t, askOnce, func(confID string) *genai.Content {
		return verdictResponse(confID, map[string]any{"confirmed": true})
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want exactly 1: %+v", len(p.executions), p.executions)
	}
	if got := p.executions[0]; got.Deployment != "api" || got.Replicas != 10 {
		t.Errorf("executed with %+v, want the model's own arguments {api 10}", got)
	}
	if len(p.verdicts) != 2 || p.verdicts[1] == nil || !p.verdicts[1].Confirmed {
		t.Fatalf("callback verdicts = %+v, want a second call carrying Confirmed=true", p.verdicts)
	}
}

// TestSeamRejectionNeverRunsTheTool pins the reject verdict: the tool is
// not executed and the model is told, rather than the turn erroring out.
func TestSeamRejectionNeverRunsTheTool(t *testing.T) {
	p := runSeamProbe(t, askOnce, func(confID string) *genai.Content {
		return verdictResponse(confID, map[string]any{"confirmed": false})
	})

	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) after a rejection, want 0: %+v", len(p.executions), p.executions)
	}
	if len(p.verdicts) != 2 || p.verdicts[1] == nil || p.verdicts[1].Confirmed {
		t.Fatalf("callback verdicts = %+v, want a second call carrying Confirmed=false", p.verdicts)
	}
}

// TestSeamEditedArgumentsAreWhatExecutes pins fact 4 — the mechanism
// W2.5 needs. ADK re-fires the original call verbatim, so the operator's
// edit has to be applied to the args map on the way past; this asserts
// that doing so actually changes what the tool receives, and that the
// operator's payload survives the JSON round-trip through the event log.
func TestSeamEditedArgumentsAreWhatExecutes(t *testing.T) {
	p := runSeamProbe(t, askOnce, func(confID string) *genai.Content {
		return verdictResponse(confID, map[string]any{
			"confirmed": true,
			"payload": map[string]any{
				"args": map[string]any{"deployment": "api", "replicas": 2},
			},
		})
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want exactly 1: %+v", len(p.executions), p.executions)
	}
	if got := p.executions[0]; got.Deployment != "api" || got.Replicas != 2 {
		t.Fatalf("executed with %+v, want the operator's edit {api 2} — an in-place args rewrite does not reach the tool", got)
	}
}
