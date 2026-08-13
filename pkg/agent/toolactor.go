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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"os"
	"sort"
	"strings"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
)

// NewToolActorModel returns a REQUEST-DRIVEN offline fake model that
// drives registered tool calls deterministically — the double the v0.2
// end-to-end UAT (scripts/uat-v0.2.sh) needs to exercise the crash /
// drain / abort legs against a real, blocking MCP tool.
//
// Why not the scripted provider (pkg/providers/mock)? The scripted model
// replays a fixed positional list of turns behind a single global cursor
// that RESETS to 0 on every process restart. The legs this UAT targets
// restart the daemon mid-flight, and the daemon's model-call count
// differs before vs. after a restart (auto-resume drives a continuation
// turn; an ambiguous-effect session refuses the mutating call; etc.), so
// a positional script and the live call sequence drift apart across
// exactly the crash-restart boundary under test. A request-driven double
// has no cursor: it decides purely from the current request, so it is
// restart-safe, session-independent, and needs no per-leg JSONL. This is
// a documented deviation from docs/uat-v0.2-plan.md's "scripted provider"
// note (which predates local/stdio MCP); see that doc's implementation
// status.
//
// Behavior, decided per request:
//
//   - Coordinator turn (Chat mode: a sub-agent "task" tool is offered,
//     finish_task is not): if the delegation tool has not yet produced a
//     response in the history, call it once (delegate to the worker);
//     otherwise emit a short final text (the worker's result has come
//     back — the turn is done).
//   - Worker turn (Task mode: finish_task is offered): if the incident
//     envelope's reason selects a UAT tool (apply -> apply_change,
//     read -> read_status) that is registered and not yet answered in the
//     history, call it once; otherwise call finish_task. Its arguments
//     satisfy the declared output schema when the specialist declares
//     one (schemafill.go).
//   - Classifier turn (SingleTurn mode: no tools at all): reply with the
//     bare incident reason, so graph dispatch routes to the real
//     per-failure-mode specialist rather than the Default edge.
//
// Selecting the tool from the inject reason keeps each leg's control in
// the harness's payload (reason "ApplyChange" vs "ReadStatus"), not in a
// brittle out-of-band script. It is offline and credential-free.
//
// Set MAST_TOOLACTOR_DEBUG=1 to log each request's offered tool names and
// the chosen action to stderr — used when adapting the fixture to a new
// ADK tool-naming convention.
func NewToolActorModel(name string) model.LLM {
	return &toolActor{name: name, debug: os.Getenv("MAST_TOOLACTOR_DEBUG") != ""}
}

type toolActor struct {
	name  string
	debug bool
}

func (m *toolActor) Name() string { return m.name }

// uatTools are the MCP tools the fixture registers, in the order the
// reason keyword is matched against them.
var uatTools = []struct{ keyword, tool string }{
	{"apply", "apply_change"},
	{"read", "read_status"},
}

func (m *toolActor) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		toolNames := toolKeys(req)
		reason := reasonAcross(req)
		if m.debug {
			m.logRequest(req, toolNames, reason)
		}

		usage := &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     int32(min(promptLen(req)/4, math.MaxInt32)), // #nosec G115 -- clamped to MaxInt32
			CandidatesTokenCount: 16,
		}
		usage.TotalTokenCount = usage.PromptTokenCount + usage.CandidatesTokenCount

		_, hasFinish := req.Tools["finish_task"]
		if hasFinish {
			// Worker (Task) turn: call the reason-selected UAT tool once,
			// else finish. The reason arrives here as the delegation tool's
			// "request" arg (plain text, not the JSON incident envelope), so
			// the keyword is matched against ALL request text — reasonAcross's
			// JSON regex does not see it on this turn.
			haystack := strings.ToLower(allText(req))
			for _, ut := range uatTools {
				if _, ok := req.Tools[ut.tool]; !ok {
					continue
				}
				if strings.Contains(haystack, ut.keyword) && !responded(req, ut.tool) {
					// The blocker's tools take no arguments
					// (parametersJsonSchema is an empty, closed object), so
					// emit an empty arg map — an extra property would fail
					// schema validation.
					yield(functionCall(ut.tool, map[string]any{}, usage), nil)
					return
				}
			}
			// finish_task's arguments come from its declaration: a
			// specialist that declares `output_schema:` is handed that
			// schema as its finish_task parameters, and a call that
			// violates it is refused rather than accepted (schemafill.go).
			if schemaViolationGiveUp(req) {
				yield(&model.LLMResponse{
					Content:       genai.NewContentFromText("[toolactor] giving up: the report contract was refused", genai.RoleModel),
					UsageMetadata: usage,
					TurnComplete:  true,
					FinishReason:  genai.FinishReasonStop,
				}, nil)
				return
			}
			args := finishTaskArgs(req, reason, fmt.Sprintf("[toolactor] handled %s", reason))
			yield(functionCall("finish_task", args, usage), nil)
			return
		}

		// Coordinator (Chat) turn: delegate to the worker's task tool once,
		// then answer.
		if deleg := delegationTool(toolNames); deleg != "" && !responded(req, deleg) {
			yield(functionCall(deleg, map[string]any{"request": reason}, usage), nil)
			return
		}
		// No tools at all: a SingleTurn agent — mast's LLM-as-router
		// classifier. Reply with the bare incident reason so graph
		// dispatch routes to the real per-failure-mode specialist
		// offline, the same convention the echo model follows. Without
		// this every toolactor run fell to the Default (_fallback) edge,
		// so a routing regression could not be seen.
		if len(toolNames) == 0 && reason != "" {
			yield(&model.LLMResponse{
				Content:       genai.NewContentFromText(reason, genai.RoleModel),
				UsageMetadata: usage,
				TurnComplete:  true,
				FinishReason:  genai.FinishReasonStop,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:       genai.NewContentFromText(fmt.Sprintf("[toolactor] done: %s", reason), genai.RoleModel),
			UsageMetadata: usage,
			TurnComplete:  true,
			FinishReason:  genai.FinishReasonStop,
		}, nil)
	}
}

// functionCall wraps a single FunctionCall response.
func functionCall(name string, args map[string]any, usage *genai.GenerateContentResponseUsageMetadata) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
		},
		UsageMetadata: usage,
		TurnComplete:  true,
		FinishReason:  genai.FinishReasonStop,
	}
}

// delegationTool returns the single sub-agent "task" tool ADK installs on
// the coordinator: the tool name that is neither finish_task nor one of
// the fixture's MCP tools. Empty when none is offered.
func delegationTool(names []string) string {
	known := map[string]bool{"finish_task": true, "apply_change": true, "read_status": true}
	for _, n := range names {
		if !known[n] {
			return n
		}
	}
	return ""
}

// responded reports whether a FunctionResponse for tool already appears in
// the request history — i.e. the call has been made and answered.
func responded(req *model.LLMRequest, tool string) bool {
	if req == nil {
		return false
	}
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == tool {
				return true
			}
		}
	}
	return false
}

// toolKeys returns the sorted names of the tools offered in the request.
func toolKeys(req *model.LLMRequest) []string {
	if req == nil {
		return nil
	}
	out := make([]string, 0, len(req.Tools))
	for k := range req.Tools {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reasonAcross scans the whole history for the incident reason (the UAT
// payload carries it in the INJECT envelope). Unlike echo's last-message
// scan, a worker turn's most recent message may be a tool response, so
// the reason must be recovered from anywhere in the history.
func reasonAcross(req *model.LLMRequest) string {
	if req == nil {
		return ""
	}
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				if r := firstMatch(reasonRe, p.Text); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

// allText concatenates every text part in the request history. The worker
// turn recovers its tool selection from here because the reason reaches it
// as the delegation tool's plain-text "request" arg, not the JSON envelope.
func allText(req *model.LLMRequest) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func promptLen(req *model.LLMRequest) int {
	return len(lastUserText(req))
}

func (m *toolActor) logRequest(req *model.LLMRequest, names []string, reason string) {
	decls := "[]"
	if req != nil && req.Config != nil {
		if b, err := json.Marshal(req.Config.Tools); err == nil {
			decls = string(b)
		}
	}
	fmt.Fprintf(os.Stderr, "toolactor: tools=%v reason=%q decls=%s\n", names, reason, decls)
}
