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

package agent_test

import (
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	mastagent "github.com/go-steer/mast/pkg/agent"
)

// TestFinishTaskIsStillCalledThat pins the one string FinishOnStall cannot
// look up. ADK's own constant lives in internal/workflowinternal, so mast
// spells it by hand; if ADK ever renames the tool, the injected call would
// name a tool that does not exist, the delegation would never close, and the
// guard would fail in exactly the situation it was installed for. Reading the
// name off a real Task sub-agent's declarations is the only check available.
func TestFinishTaskIsStillCalledThat(t *testing.T) {
	m := buildSpecialist(t, mastagent.TaskAgentConfig{})
	if !declared(t, m)[mastagent.FinishTaskToolName] {
		t.Fatalf("a Task sub-agent is not offered %q; FinishOnStall would inject a call to a "+
			"tool that does not exist. Declared: %v", mastagent.FinishTaskToolName, names(declared(t, m)))
	}
}

// finishTaskResponse is a whole turn in one response: the Task agent reports
// and stops.
func finishTaskResponse(result string) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(mastagent.FinishTaskToolName, map[string]any{"result": result})},
		},
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
}

// TestModelCallbacksReachTheAgent is the plumbing half of the seam, and the
// only thing it is really testing is that the new fields are not silently
// dropped — a config field that is accepted and ignored is worse than one that
// does not exist, because it reads as configured.
//
// Each mode is asserted on its own, because ADK runs them exclusively: a
// Before callback that returns a response yields it and returns, so the model
// and every After callback are skipped (internal/llminternal/base_flow.go:785).
// Asserting both from one run would prove one of them and look like both.
func TestModelCallbacksReachTheAgent(t *testing.T) {
	t.Run("a Before callback short-circuits the model call", func(t *testing.T) {
		before := func(_ adkagent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
			return finishTaskResponse("short-circuited"), nil
		}
		m := buildSpecialist(t, mastagent.TaskAgentConfig{
			BeforeModelCallbacks: []llmagent.BeforeModelCallback{before},
		})
		if len(m.systems) != 0 {
			t.Errorf("the model was called %d times despite a short-circuiting BeforeModelCallback; "+
				"the callback never reached the agent", len(m.systems))
		}
	})

	t.Run("an After callback replaces the model's response", func(t *testing.T) {
		var saw int
		after := func(_ adkagent.Context, resp *model.LLMResponse, _ error) (*model.LLMResponse, error) {
			saw++
			// Replacing rather than observing: an After callback that is
			// invoked but whose return value is discarded would pass a
			// call-count assertion and fail FinishOnStall.
			return finishTaskResponse("replaced"), nil
		}
		m := buildSpecialist(t, mastagent.TaskAgentConfig{
			AfterModelCallbacks: []llmagent.AfterModelCallback{after},
		})
		if len(m.systems) == 0 {
			t.Fatal("the model was never called, so nothing could have been replaced")
		}
		if saw == 0 {
			t.Fatal("the AfterModelCallback was never invoked; it never reached the agent")
		}
		// The specialist would otherwise have answered twice — specialistScript
		// calls finish_task on every turn — so one call means the replacement
		// is what the flow acted on.
		if len(m.systems) != 1 {
			t.Errorf("the model was called %d times, want 1: the replacement response "+
				"did not end the task", len(m.systems))
		}
	})

	t.Run("a coordinator's callbacks reach it too", func(t *testing.T) {
		var saw int
		after := func(_ adkagent.Context, _ *model.LLMResponse, _ error) (*model.LLMResponse, error) {
			saw++
			return nil, nil
		}
		root, err := mastagent.NewCoordinator(mastagent.CoordinatorConfig{
			Name:                "coord",
			Description:         "Coordinator under test.",
			Model:               &recordingModel{name: "rec-coord", respond: coordinatorScript()},
			AfterModelCallbacks: []llmagent.AfterModelCallback{after},
		})
		if err != nil {
			t.Fatalf("NewCoordinator: %v", err)
		}
		runTree(t, root)
		if saw == 0 {
			t.Error("CoordinatorConfig.AfterModelCallbacks never reached the agent")
		}
	})
}

// silentTurn is what a stalled agent's last response looks like: text, no
// function call, nothing left to run.
func silentTurn(text string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      genai.NewContentFromText(text, genai.RoleModel),
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
}

// injected returns the function call FinishOnStall appended to resp, or "" if
// the callback declined to act (a nil return leaves the response alone).
func injected(t *testing.T, cb llmagent.AfterModelCallback, resp *model.LLMResponse, err error) *genai.FunctionCall {
	t.Helper()
	out, cbErr := cb(nil, resp, err)
	if cbErr != nil {
		t.Fatalf("FinishOnStall returned an error: %v", cbErr)
	}
	if out == nil {
		return nil
	}
	for _, p := range out.Content.Parts {
		if p != nil && p.FunctionCall != nil {
			return p.FunctionCall
		}
	}
	return nil
}

func TestFinishOnStallClosesASilentTurn(t *testing.T) {
	cb := mastagent.FinishOnStall("pod_inspector", nil)
	fc := injected(t, cb, silentTurn("Could you run kubectl get all and paste the output?"), nil)
	if fc == nil {
		t.Fatal("a terminal turn with no function call was left alone; the delegation would never close")
	}
	if fc.Name != mastagent.FinishTaskToolName {
		t.Errorf("injected call is %q, want %q", fc.Name, mastagent.FinishTaskToolName)
	}
	if fc.ID != "" {
		t.Errorf("injected call carries ID %q; it must be left empty for ADK to stamp", fc.ID)
	}

	result, _ := fc.Args["result"].(string)
	if !mastagent.Stalled(result) {
		t.Fatalf("the payload is not recognisable as a stall: %q", result)
	}
	// The two things a caller needs out of it: which agent gave up, and what it
	// was trying to say. The last words are usually the most informative thing
	// in the delegation — the question names the data the agent could not get.
	if !strings.Contains(result, "pod_inspector") {
		t.Errorf("the payload does not name the agent: %q", result)
	}
	if !strings.Contains(result, "kubectl get all") {
		t.Errorf("the agent's last words were dropped: %q", result)
	}
}

// The response is amended, not replaced. Providers that sign thinking blocks
// require them replayed intact, so dropping the original parts would corrupt
// the agent's own history on the next turn.
func TestFinishOnStallKeepsTheOriginalParts(t *testing.T) {
	resp := &model.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{Text: "reasoning", Thought: true},
				{Text: "I need more data."},
			},
		},
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
	out, err := mastagent.FinishOnStall("a", nil)(nil, resp, nil)
	if err != nil {
		t.Fatalf("FinishOnStall: %v", err)
	}
	if out == nil {
		t.Fatal("the callback declined to act on a silent turn")
	}
	if got, want := len(out.Content.Parts), 3; got != want {
		t.Fatalf("response has %d parts, want %d (the originals plus the injected call)", got, want)
	}
	if !out.Content.Parts[0].Thought {
		t.Error("the thinking block was not preserved in place")
	}
	// The original response is not mutated: ADK may hold it elsewhere.
	if len(resp.Content.Parts) != 2 {
		t.Errorf("the input response was mutated: it now has %d parts", len(resp.Content.Parts))
	}
	// And a thinking block is not the agent's last words.
	result, _ := out.Content.Parts[2].FunctionCall.Args["result"].(string)
	if strings.Contains(result, "reasoning") {
		t.Errorf("the payload leaked a thinking block: %q", result)
	}
}

// The turns the guard must not touch. Each of these would be a live bug: the
// first two would double-report, and the Interrupted case would resolve a
// human-approval pause on the human's behalf.
func TestFinishOnStallLeavesLiveTurnsAlone(t *testing.T) {
	call := &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall("k8s_cluster_health", map[string]any{})},
		},
		TurnComplete: true,
	}
	finish := &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(mastagent.FinishTaskToolName, map[string]any{"result": "done"})},
		},
		TurnComplete: true,
	}
	partial := silentTurn("half a sen")
	partial.Partial = true
	interrupted := silentTurn("waiting for approval")
	interrupted.Interrupted = true
	failed := silentTurn("")
	failed.ErrorCode = "SAFETY"
	empty := &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel}}

	cases := []struct {
		name string
		resp *model.LLMResponse
		err  error
	}{
		{"a tool call means the turn continues", call, nil},
		{"the ordinary finish_task path", finish, nil},
		{"a streamed fragment is not a finished turn", partial, nil},
		{"an interrupt is a legitimate pause", interrupted, nil},
		{"an error response is not a report", failed, nil},
		{"a response with no parts", empty, nil},
		{"a model error", silentTurn("x"), errModel},
		{"a nil response", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if fc := injected(t, mastagent.FinishOnStall("a", nil), c.resp, c.err); fc != nil && fc.Name == mastagent.FinishTaskToolName {
				t.Errorf("the guard injected %s", mastagent.FinishTaskToolName)
			}
		})
	}
}

var errModel = errStr("the model failed")

type errStr string

func (e errStr) Error() string { return string(e) }

// A caller-supplied payload is used verbatim. This is the case that matters
// for any roster with an output_schema: mast must not substitute its own
// shape, because the runtime validates the injected call against the schema
// exactly as it validates a model-issued one.
func TestFinishOnStallUsesTheCallersPayload(t *testing.T) {
	payload := func(agentName, lastWords string) map[string]any {
		return map[string]any{
			"severity": "ok",
			"findings": []any{},
			"summary":  mastagent.StallText(agentName, lastWords),
		}
	}
	fc := injected(t, mastagent.FinishOnStall("auditor", payload), silentTurn("no telemetry"), nil)
	if fc == nil {
		t.Fatal("the callback declined to act on a silent turn")
	}
	if _, ok := fc.Args["result"]; ok {
		t.Errorf("the default payload was used despite a caller-supplied one: %v", fc.Args)
	}
	if got, ok := fc.Args["findings"].([]any); !ok || len(got) != 0 {
		t.Errorf("findings = %v, want the caller's empty list", fc.Args["findings"])
	}
	summary, _ := fc.Args["summary"].(string)
	if !mastagent.Stalled(summary) {
		t.Errorf("StallText did not put the marker where Stalled can find it: %q", summary)
	}
}

// Stalled is the counting seam, so it has to be exact: an ordinary report that
// merely discusses an incomplete check must not be counted as a stall.
func TestStalledMatchesOnlyTheMarkerAtTheHead(t *testing.T) {
	if !mastagent.Stalled("  " + mastagent.StallText("a", "")) {
		t.Error("Stalled rejected its own text")
	}
	if mastagent.Stalled("The rollout is incomplete. " + mastagent.StallMarker) {
		t.Error("Stalled matched a marker that is not at the head of the text")
	}
	if mastagent.Stalled("Checked every namespace; nothing incomplete.") {
		t.Error("Stalled matched an ordinary report")
	}
}
