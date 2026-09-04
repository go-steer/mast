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

// The refused agent's last call: report, do not investigate.
//
// precall.go's package comment states the prohibition this file works
// under. mast will not fabricate a schema-conforming report for a
// refused agent, because a refusal is the case where nothing was looked
// at and an invented finding would enter an incident stream as a real
// fault. Nothing here relaxes that: mast still synthesizes no content.
//
// What it does is notice that the premise can be false. An agent
// refused on its first call looked at nothing. An agent refused after
// six log queries and 269k tokens (#271) looked at a great deal, and
// throwing that away is not caution — the tokens were spent either way,
// and the operator gets an incident with nothing attached. The fix is
// to let the MODEL write the report it was about to write anyway, in
// one more call, with nothing to investigate with.
//
// That last clause is the whole shape. The grant strips every tool but
// finish_task from the request, so the call cannot go looking for
// anything further; the only move left is to report. What comes back is
// the model's own account of what it established, in its own schema,
// including "I established nothing" if that is the truth.
//
// The grant is the gate's to give, and *budget.Meter gives it at most
// once per agent, only when opted in, and never to an agent with no
// spend (pkg/budget/finalreport.go). This file is the request half: it
// takes a granted refusal and turns the pending call into a report-only
// one.

package agent

import (
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// FinalReportInstruction is what the refused agent is told on its last
// call.
//
// It has to do two jobs at once, and the second is the harder one. It
// must get a report out of a model whose plan was to keep looking — so
// it says plainly that there is no next call. And it must not turn a
// thin investigation into a confident answer: an agent that says more
// than it checked is worse than one that says nothing, because a
// half-finding reads exactly like a whole one once it is in an incident
// stream.
const FinalReportInstruction = "STOP INVESTIGATING. You have reached your budget " +
	"ceiling and this is your final model call: every tool except your report tool " +
	"has been withdrawn, and there will be no further turn. Submit your report now, " +
	"covering only what you have already established from the evidence above. State " +
	"explicitly which parts of your assignment you did not get to, and mark any " +
	"conclusion you could not verify as unverified. Do not speculate to fill the gaps " +
	"— an incomplete report that is honest about its gaps is what is wanted here."

// FinalReportGate is the optional half of CallGate: a gate that can
// grant a refused agent one last call to report with.
//
// Kept separate from CallGate rather than added to it, because it is
// genuinely optional. A kill switch or a maintenance window has no
// notion of "one more call" and should not have to implement one to
// satisfy the interface; RefuseOnGate type-asserts for this and refuses
// in the ordinary way when the gate does not offer it.
type FinalReportGate interface {
	// AllowFinalReport reports whether agentName may make one model call
	// past the refusal, to write its report — and consumes the grant, so
	// a second ask for the same agent returns false.
	//
	// It is called only after Allow has already refused, so an
	// implementation may treat it as "the ceiling just fired". It must be
	// safe for concurrent use, for the reason Allow is.
	AllowFinalReport(agentName string) bool
}

// finalReportTools returns req's tool declarations narrowed to
// finish_task alone, or nil if the request has no report channel to
// narrow to.
//
// Asked before the gate is, and separately from applying it, because
// the gate consumes the grant when it answers: a request that turned
// out to be unreportable would otherwise burn an agent's one chance and
// still refuse it.
//
// The check is against the wire declarations rather than req.Tools: the
// declarations are what the provider is shown, so a request whose
// finish_task never reached Config.Tools would spend the grant on a
// call the model cannot report through.
func finalReportTools(req *model.LLMRequest) []*genai.Tool {
	if req == nil || req.Config == nil {
		return nil
	}
	return keepOnlyFinishTask(req.Config.Tools)
}

// applyFinalReport rewrites req into the report-only call described
// above, given the narrowed tools finalReportTools already found.
func applyFinalReport(req *model.LLMRequest, tools []*genai.Tool, reason string) {
	req.Config.Tools = tools

	// req.Tools is what the flow executes the returned call against, and
	// it is pruned to match so the two halves cannot disagree about what
	// this turn offers.
	if fn, ok := req.Tools[FinishTaskToolName]; ok {
		req.Tools = map[string]any{FinishTaskToolName: fn}
	}

	// Force the call rather than merely offering it. A model that
	// answers this instruction in prose produces no report, and the
	// grant — a real model call, charged past a ceiling the operator
	// declared — buys nothing. Both providers honour a single allowed
	// name: pkg/providers/anthropic maps it to a specific tool choice,
	// and Gemini takes it directly.
	//
	// This can still fail, and the failure is bounded rather than
	// prevented: a forced call whose arguments do not satisfy the output
	// schema draws ADK's retry instruction, the retry asks this seam
	// again, and the gate — which latched the grant when it gave it —
	// refuses. One wasted call, not the 3,292-call spin of W10.3.
	if req.Config.ToolConfig == nil {
		req.Config.ToolConfig = &genai.ToolConfig{}
	}
	req.Config.ToolConfig.FunctionCallingConfig = &genai.FunctionCallingConfig{
		Mode:                 genai.FunctionCallingConfigModeAny,
		AllowedFunctionNames: []string{FinishTaskToolName},
	}

	// The instruction goes in as a user turn rather than onto the system
	// instruction, because it is news: it describes what just happened
	// to this request, not a standing fact about the agent. A system
	// instruction assembled before the turn began is also the one thing
	// a provider cache may be holding (pkg/providers/gemini/builtins.go),
	// and rewriting it here would miss the cache for a single call whose
	// entire point is to be cheap.
	text := FinalReportInstruction
	if reason = strings.TrimSpace(reason); reason != "" {
		text += "\n\nReason: " + reason
	}
	req.Contents = append(req.Contents, &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: text}},
	})
}

// keepOnlyFinishTask returns tools with every declaration but
// finish_task removed, dropping any genai.Tool left with nothing in it.
//
// It rebuilds rather than filtering in place: the slice and the
// declarations under it are assembled per-request by ADK, but a
// provider-side builtin loader appends to the same slice
// (pkg/providers/gemini/builtins.go) and saves it for restore, so
// mutating the backing array would edit what that restore hands back.
//
// Non-function tools — a provider builtin like Google Search — are
// dropped with everything else. They are exactly the kind of "one more
// look" this call is not for.
func keepOnlyFinishTask(tools []*genai.Tool) []*genai.Tool {
	var out []*genai.Tool
	for _, t := range tools {
		if t == nil {
			continue
		}
		var decls []*genai.FunctionDeclaration
		for _, fd := range t.FunctionDeclarations {
			if fd != nil && fd.Name == FinishTaskToolName {
				decls = append(decls, fd)
			}
		}
		if len(decls) == 0 {
			continue
		}
		out = append(out, &genai.Tool{FunctionDeclarations: decls})
	}
	return out
}
