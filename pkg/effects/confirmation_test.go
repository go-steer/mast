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

package effects

import (
	"context"
	"testing"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/mast/pkg/transcript"
)

// confirmProbe runs the ADK tool-confirmation HITL shape
// (agent.Context.RequestConfirmation — one of the two tool-level HITL
// shapes mast's own planner doc names) end to end, with the outbox
// plugin optionally attached, and reports how many times the gated
// mutating tool actually executed.
func confirmProbe(t *testing.T, withOutbox bool) int {
	t.Helper()
	svc := sqliteService(t)
	store := transcript.NewStore(svc, testApp)

	executed := 0
	scaleTool, err := functiontool.New(functiontool.Config{
		Name:        "scale_up",
		Description: "mutating tool gated behind an operator confirmation",
	}, func(ctx adkagent.Context, args scaleArgs) (map[string]any, error) {
		if c := ctx.ToolConfirmation(); c == nil {
			if err := ctx.RequestConfirmation("scale the deployment?", map[string]any{"delta": args.Delta}); err != nil {
				return nil, err
			}
			return map[string]any{"status": "awaiting operator confirmation"}, nil
		} else if !c.Confirmed {
			return map[string]any{"status": "rejected"}, nil
		}
		executed++
		return map[string]any{"scaled": args.Delta}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	m := &scriptedModel{name: "confirm", script: roundScript(
		callResponse("scale_up", map[string]any{"delta": 3}),
	)}
	root, err := llmagent.New(llmagent.Config{
		Name:        "confirm_agent",
		Description: "confirmation probe",
		Instruction: "act",
		Model:       m,
		Tools:       []tool.Tool{scaleTool},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	rc := runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
	}
	if withOutbox {
		p, err := New(Config{
			Predicate:     NewPredicate(nil),
			SubAgentNames: SubAgentNames(root),
			AckedAt: func(ctx context.Context, sid string) (time.Time, bool) {
				return store.EffectsAckedAt(ctx, "", sid)
			},
		})
		if err != nil {
			t.Fatalf("effects.New: %v", err)
		}
		rc.PluginConfig = runner.PluginConfig{Plugins: []*plugin.Plugin{p}}
	}
	r, err := runner.New(rc)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	const sid = "confirm-session"
	run := func(msg *genai.Content) {
		t.Helper()
		for _, err := range r.Run(context.Background(), testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("runner.Run: %v", err)
			}
		}
	}

	// Turn 1: the tool asks for confirmation and the flow pauses.
	run(genai.NewContentFromText("scale it up", genai.RoleUser))

	// Locate the adk_request_confirmation function call the flow emitted.
	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	confID := ""
	origHasResponse := false
	for ev := range got.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil && p.FunctionCall.Name == toolconfirmation.FunctionCallName {
				confID = p.FunctionCall.ID
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "scale_up" {
				origHasResponse = true
				t.Logf("pre-pause recorded completion for scale_up: id=%s response=%v",
					p.FunctionResponse.ID, p.FunctionResponse.Response)
			}
		}
	}
	if confID == "" {
		t.Fatalf("no %s function call emitted; the probe's premise does not hold", toolconfirmation.FunctionCallName)
	}
	t.Logf("confirmation call id=%s; original scale_up call has a recorded FunctionResponse: %v", confID, origHasResponse)

	// Turn 2: the operator approves.
	fr := genai.NewPartFromFunctionResponse(toolconfirmation.FunctionCallName, map[string]any{
		"confirmed": true,
		"hint":      "scale the deployment?",
	})
	fr.FunctionResponse.ID = confID
	run(&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{fr}})

	return executed
}

// TestConfirmationResumeIsNotFalselyReplayed pins adversarial finding
// N1: the confirmation-gated HITL shape persists a PLACEHOLDER
// FunctionResponse under the original call ID before pausing, and the
// operator-approved re-execution re-fires under that same ID — the
// outbox must not replay the placeholder in place of the approved
// mutation. The probe runs the flow with and without the plugin; both
// must execute the approved tool exactly once.
func TestConfirmationResumeIsNotFalselyReplayed(t *testing.T) {
	baseline := confirmProbe(t, false)
	if baseline != 1 {
		t.Fatalf("baseline (no outbox): approved tool executed %d time(s), want 1 — the probe's premise does not hold", baseline)
	}
	withPlugin := confirmProbe(t, true)
	if withPlugin != 1 {
		t.Fatalf("with outbox: approved tool executed %d time(s), want 1 — the placeholder response was falsely replayed as a completion", withPlugin)
	}
}
