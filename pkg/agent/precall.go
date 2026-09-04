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

// Stopping a call before it is paid for.
//
// A ceiling that is only checked after the fact is a ceiling plus one
// call. RefuseOnGate is the seam that asks first: a BeforeModelCallback
// that consults a CallGate and, when the gate refuses, returns a
// response instead of letting the request reach the provider.
//
// Why this seam and not a model.LLM wrapper is settled in v0.6 W10.1
// (docs/README.md's resolved-decisions table, measured in
// pkg/planner/precallseam_test.go). The short version: both reach a
// planner-dispatched specialist, but a ceiling is keyed on the agent
// name, and a callback is handed agent.Context as a declared parameter
// while a wrapper would have to recover the name by asserting on a
// context.Context that ADK merely happens to populate.
//
// # The gate travels on the context
//
// A gate is not installed on the agent, because agents are built once
// per daemon and a ceiling belongs to a session. It rides the Go
// context instead, put there by whoever starts the turn, and that
// choice is load-bearing rather than stylistic: it is what makes the
// seam work inside a planner dispatch. Resolving a meter from
// ctx.SessionID() would not — invoke_specialist runs its sub-runner
// under "invoke-<name>", so a session-keyed lookup would miss the
// operator's meter, mint an empty one, and approve every call. The
// context survives the boundary intact because the dispatch tool
// derives its sub-context from the tool context with WithAgentCancel.
//
// A call with no gate on its context is not refused. "Unmetered" and
// "metered with no ceilings" are different statements and neither one
// stops a workload; a seam that failed closed on the first would break
// every library embedder who never asked for a budget.
//
// # A refusal is a response, not an error
//
// The other half of W10.1. Both shapes skip the provider, so both are
// genuinely pre-call, but an error returned from here reaches the
// caller in the field ADK uses for a tool that broke — measured on the
// dispatch path, a refusal returned as an error arrives at the planner
// as {"error": "... workflow: dynamic child failed: ..."}. pkg/planner
// already refused to emit that shape for a crossed cap, on the grounds
// that a cap that fires must not look like a broken tool, and a check
// one layer down should not contradict it.
//
// So the refusal is synthesized, exactly as FinishOnStall synthesizes
// one for an agent that went quiet — same mechanism, different reason.
// Which synthesis depends on what the agent can say: an agent that
// declares finish_task reports through it, and the caller reads an
// ordinary result. An agent that does not simply answers with the
// refusal text and ends its turn, because there is nobody above it to
// route around the ceiling.
//
// # An agent with no valid report shape answers in plain text
//
// A third case, and the one that has teeth. An agent that declares an
// OutputSchema has a finish_task whose parameters are that schema, so
// the default {"result": string} payload does not validate — and ADK
// answers an invalid finish_task with a retry instruction rather than
// an error, which is a loop this seam feeds for free: refused call,
// invalid report, retry, refused call. Measured before it was fixed, a
// v0.4 UAT leg spun to 3,292 finish_task calls in ninety seconds.
//
// mast will not close the loop by fabricating a conforming value. The
// schema describes findings and a refusal is exactly the case where
// nothing was looked at, so a synthesized one would put an invented
// fault into an incident stream. Nor will it stop the session, which is
// what W10.3 removed. It falls back to the plain-text answer above: the
// agent's run ends, its delegation resolves to nothing, and the fact
// that nothing came back is carried by the four surfaces that count a
// trip rather than by a report that would have to be made up. A host
// that can express "not done" in its own schema should say so by
// supplying a RefusalPayload; UnreportableRefusal is what it gets if it
// does not.
//
// # Unless the agent had already found something
//
// The paragraph above rests on "nothing was looked at", and that
// premise fails for an agent stopped mid-investigation rather than at
// its first call (#271). A gate that implements FinalReportGate can
// grant such an agent one more model call with every tool but
// finish_task withdrawn, so the report is written by the model out of
// what it actually saw. Nothing above is relaxed: mast still fabricates
// no content. See finalreport.go.

package agent

import (
	"context"
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// RefusalMarker opens the text a refused agent reports.
//
// Exported for the same reason StallMarker is: a run that ended short of
// its work has to be countable, and a refusal that nothing counts is
// indistinguishable from a workload that simply had less to do. Use
// Refused to test for it rather than matching the string.
const RefusalMarker = "STOPPED — this agent reached a cost ceiling."

// RefusalInstruction is what the caller is told to do about it.
//
// It says three things, each of which is a way a reader could draw the
// wrong conclusion from a refusal: the agent's part of the work is
// unchecked rather than clean, the ceiling is a fact about the budget
// and not about the incident, and asking again costs the same refusal
// because nothing about the ceiling changes by being retried.
const RefusalInstruction = "This agent was stopped before its next model call " +
	"because a budget ceiling could not be respected by making it. Its part of the " +
	"work is unchecked: do not infer that it found nothing, and say plainly in your " +
	"own answer what was not completed. Delegating the same question to it again " +
	"will be refused the same way. If other paths remain within budget, use them."

// Refused reports whether text is something RefuseOnGate wrote.
//
// The counting seam, and also the seam a coordinator's own summary
// logic should use to tell "this specialist had nothing to report" from
// "this specialist was never allowed to look".
func Refused(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), RefusalMarker)
}

// RefusalText composes the marker, the agent's name, the instruction and
// the gate's own reason — which carries the arithmetic, so an operator
// reading a transcript can see which ceiling and by how much without
// opening the meter.
func RefusalText(agentName, reason string) string {
	text := fmt.Sprintf("%s (%s) %s", RefusalMarker, agentName, RefusalInstruction)
	if reason = strings.TrimSpace(reason); reason != "" {
		text += "\n\nReason: " + reason
	}
	return text
}

// CallGate decides whether an agent may make another model call.
//
// The one implementation that matters is *budget.Meter, whose Allow has
// exactly this shape. It is an interface here so that pkg/agent does not
// depend on pkg/budget for a one-method question, and so a host with its
// own notion of "may this call happen" — a quota, a kill switch, a
// maintenance window — can install one without going through the meter.
//
// Allow must be safe for concurrent use: a fan-out roster asks it from
// every branch at once.
type CallGate interface {
	// Allow returns nil if agentName may make another model call now, or
	// the reason it may not. The reason is shown to the model and
	// written into the transcript, so it should read as an explanation
	// rather than as a stack trace.
	Allow(agentName string) error
}

type callGateKey struct{}

// WithCallGate returns ctx carrying gate, for RefuseOnGate to find.
//
// Call it once where a turn starts, on the context handed to
// runner.Run. Everything below inherits it, including a planner
// dispatch's private sub-runner.
//
// A nil gate returns ctx unchanged rather than installing an absence, so
// a host that conditionally meters does not have to branch at the call
// site.
func WithCallGate(ctx context.Context, gate CallGate) context.Context {
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, callGateKey{}, gate)
}

// CallGateFrom returns the gate on ctx, or nil if the call is unmetered.
func CallGateFrom(ctx context.Context) CallGate {
	gate, _ := ctx.Value(callGateKey{}).(CallGate)
	return gate
}

// RefusalPayload builds the finish_task arguments RefuseOnGate submits
// for a refused agent, given the agent's name and the gate's reason. It
// exists for the reason StallPayload does, and carries the same
// obligation: ADK validates an injected call exactly as it validates a
// model-issued one, so the value has to satisfy the agent's
// OutputSchema.
//
// And it carries the same prohibition, more sharply. mast could
// synthesise a schema-conforming value (see conformingArgs in
// schemafill.go) and must not: that invents content, and a refusal is
// precisely the case where nothing was looked at. A fabricated finding
// here would enter an incident stream as a real fault discovered by an
// agent that never ran.
//
// Whatever it returns should carry RefusalMarker at the head of the
// field a reader will see, so Refused can find it.
type RefusalPayload func(agentName, reason string) map[string]any

// DefaultRefusalPayload is the payload for an agent that declares no
// OutputSchema, where ADK's own finish_task takes a single required
// string named "result". Same one case DefaultStallPayload covers, and
// for the same reason: the schema is ADK's, it has one field, and there
// is nothing to fabricate.
func DefaultRefusalPayload(agentName, reason string) map[string]any {
	return map[string]any{"result": RefusalText(agentName, reason)}
}

// UnreportableRefusal is the RefusalPayload for an agent whose report
// shape mast cannot fill honestly: it returns no arguments, which
// RefuseOnGate reads as "do not submit a report at all" and answers in
// plain text instead.
//
// Returning nil is a decision and not a gap. The alternative is a
// finish_task call the tool rejects, and ADK's rejection is a retry
// instruction rather than an error, so the agent asks again, is refused
// again, and submits the same invalid report — a loop that costs
// nothing per iteration and therefore never ends. An unresolved
// delegation is a worse answer than a good report and a much better one
// than a spin.
func UnreportableRefusal(agentName, reason string) map[string]any { return nil }

// RefuseOnGate returns a BeforeModelCallback that asks the context's
// CallGate before every model call and, when the gate refuses,
// synthesizes the agent's answer instead of issuing the call.
//
// Install it through TaskAgentConfig.BeforeModelCallbacks,
// CoordinatorConfig.BeforeModelCallbacks, or across a roster through
// specialists.BuildOptions. A nil payload means DefaultRefusalPayload,
// which is correct only for an agent with no OutputSchema; see
// RefusalPayload.
//
// # It fails open, and that is the cost of this seam
//
// A construction site that forgets to install this callback issues its
// calls unchecked. That is the one advantage the rejected model.LLM
// wrapper had — a wrapper cannot be forgotten, because there is one
// place models are built. The trade was taken knowingly in W10.1
// because the wiring is a finite, enumerable set of sites with a test
// per site, while the wrapper's weakness is a dependency's undocumented
// behaviour that no test of mast's could keep honest.
func RefuseOnGate(payload RefusalPayload) llmagent.BeforeModelCallback {
	if payload == nil {
		payload = DefaultRefusalPayload
	}
	return func(ctx adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		gate := CallGateFrom(ctx)
		if gate == nil {
			return nil, nil
		}
		name := ctx.AgentName()
		err := gate.Allow(name)
		if err == nil {
			return nil, nil
		}

		// A Task agent reports through finish_task, and reporting is
		// what lets the caller route around it. An agent without the
		// tool has no such channel, so it answers in plain text and its
		// turn ends there.
		//
		// Read off the request rather than off configuration, because
		// the request is what the flow will actually accept: injecting
		// a call to a tool this turn did not declare turns a refusal
		// into an unknown-tool error, which is the broken-tool shape
		// this whole design exists to avoid.
		_, hasFinish := req.Tools[FinishTaskToolName]

		// Before synthesizing anything: a gate that grants a final report
		// would rather have the model write one. The request becomes
		// report-only and the call proceeds — see finalreport.go for why
		// that is not the fabrication the paragraph above forbids.
		if reportTools := finalReportTools(req); hasFinish && len(reportTools) > 0 {
			if g, ok := gate.(FinalReportGate); ok && g.AllowFinalReport(name) {
				applyFinalReport(req, reportTools, err.Error())
				return nil, nil
			}
		}

		args := map[string]any(nil)
		if hasFinish {
			args = payload(name, err.Error())
		}
		// No channel to report through, or nothing valid to put in it.
		// Both end the agent's turn with the refusal as its answer; see
		// the package comment on UnreportableRefusal for why the second
		// one is not allowed to guess.
		if !hasFinish || args == nil {
			return &model.LLMResponse{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: RefusalText(name, err.Error())}},
			}}, nil
		}
		return &model.LLMResponse{Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				// ID left empty on purpose: ADK stamps one in
				// finalizeModelResponseEvent. Same as FinishOnStall.
				Name: FinishTaskToolName,
				Args: payload(name, err.Error()),
			}}},
		}}, nil
	}
}
