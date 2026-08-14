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
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// FinishTaskToolName is ADK's name for the tool a Task-mode agent reports
// through. ADK's own constant lives in internal/workflowinternal, which is
// unreachable from here; TestFinishTaskIsStillCalledThat pins the spelling
// against a real specialist's declarations, so a rename upstream fails a test
// rather than silently disarming FinishOnStall.
const FinishTaskToolName = "finish_task"

// StallMarker opens the text FinishOnStall writes on a stalled agent's behalf.
//
// Exported because a degraded run has to be countable. The whole point of the
// guard is to turn a dead run into a partial answer, and a partial answer that
// nothing counts is indistinguishable from a complete one. Use Stalled to test
// for it rather than matching the string by hand.
const StallMarker = "INCOMPLETE — no result from this agent."

// StallInstruction is what the caller is told to do about it, and it is part of
// the payload rather than of the marker because the marker is for a counter and
// this is for a model.
//
// It says three things, each of which was a way the first version went wrong: an
// empty result from a stalled agent means *not checked* rather than checked and
// clean; whatever the agent was waiting for is never coming, because a Task
// specialist on an unattended run has no interactive channel; and re-delegating
// the same question just spends the same turn again.
const StallInstruction = "This agent ended its turn without reporting, and it has " +
	"no interactive channel, so whatever it was waiting for will never arrive. Treat " +
	"its part of the work as unchecked: do not infer that it found nothing, and say " +
	"plainly in your own answer what was not completed. Do not delegate the same " +
	"question to it again. Its last message follows verbatim."

// Stalled reports whether text is something FinishOnStall wrote — the summary
// field of a synthesised result, or the whole of a default one.
//
// This is the counting seam. A caller that aggregates specialist results should
// run their text through it and report the total, the way it would report an
// error rate: a run rescued by the guard reached the end with a hole in it, and
// it must not be filed next to a run that had nothing missing.
func Stalled(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), StallMarker)
}

// StallPayload builds the finish_task arguments FinishOnStall submits for an
// agent that stopped without reporting. It is given the agent's name and its
// last words — the text of the terminal turn, thinking blocks excluded — and
// must return a value that satisfies the agent's OutputSchema, because
// ADK validates the injected call exactly as it validates a model-issued one.
//
// The payload is the caller's to write and deliberately not mast's to guess.
// A conforming value is not the same as an *empty* one, and only the roster
// that wrote the schema knows the difference: a report contract may forbid a
// severity above "ok" when the findings list is empty, in which case the
// severity is forced; a findings array must come back empty rather than filled,
// because a fabricated entry enters the caller's incident stream as a real
// fault named "a subagent stopped talking". mast has the machinery to
// synthesise a schema-conforming value — see conformingArgs in schemafill.go,
// which the offline fakes use — and using it here would be wrong for exactly
// that reason. It invents content. A stall report must invent none.
//
// Whatever it returns should carry StallMarker at the head of the field a
// reader will actually see, so Stalled can find it.
type StallPayload func(agentName, lastWords string) map[string]any

// DefaultStallPayload is the payload for an agent that declares no
// OutputSchema. ADK gives such an agent a single required string parameter
// named "result", and the marker, the instruction and the agent's last words
// all go into it.
//
// It is the default precisely because it is the one case where mast can know
// what an empty result looks like: the schema is ADK's own and it has exactly
// one field, so there is nothing to fabricate. Every other schema needs a
// StallPayload from the caller.
func DefaultStallPayload(agentName, lastWords string) map[string]any {
	return map[string]any{"result": StallText(agentName, lastWords)}
}

// StallText composes the marker, the agent's name, the instruction, and the
// agent's own last words. A StallPayload for a richer schema should use it for
// whichever field carries prose, so that the convention Stalled recognises is
// the same one everywhere.
//
// The last words are kept verbatim, and they are usually the most informative
// thing in the whole delegation: the question the agent wanted to ask names the
// data it could not get.
func StallText(agentName, lastWords string) string {
	text := fmt.Sprintf("%s (%s) %s", StallMarker, agentName, StallInstruction)
	if lastWords = strings.TrimSpace(lastWords); lastWords != "" {
		text += "\n\n" + lastWords
	}
	return text
}

// FinishOnStall converts a Task agent's silent turn into a finish_task call, so
// an agent that stops without reporting costs its own part of the answer
// instead of the whole run. Install it through TaskAgentConfig.AfterModelCallbacks,
// or across a roster through specialists.BuildOptions.OnStall.
//
// A nil payload means DefaultStallPayload, which is correct only for an agent
// with no OutputSchema; see StallPayload.
//
// # The failure this replaces
//
// ADK ends the *caller's* turn when a Task sub-agent drains its iterator
// without calling finish_task. runChat is explicit about it
// (agent/llmagent/llm_agent_wrapper.go:518):
//
//	if out == nil {
//		// Task sub-agent drained its iterator without ever
//		// calling finish_task (e.g. it emitted a natural-
//		// language question to the user and is waiting for
//		// the reply). ...
//		return false
//	}
//
// Returning false stops the outer re-entry loop, so the coordinator never
// regains control and the run produces nothing at all. ADK's reasoning is sound
// for an interactive coordinator, where the user's next message should route
// back into the paused task. It is wrong for an unattended agent: nothing will
// ever answer, and the delegation stays unresolved forever. The damage is out
// of all proportion to the cause — a coordinator that had already collected
// results from three other specialists loses those too.
//
// # Why a callback and not a prompt
//
// DefaultTaskInstruction already carries the unattended-loop discipline, and it
// is necessary and not sufficient. A specialist measured downstream asked its
// question anyway, because it had genuinely run out of moves: a prompt cannot
// forbid the only remaining action.
//
// # Why an AfterModelCallback and not a wrapper agent
//
// A Task agent cannot be wrapped from outside ADK — see the note on
// TaskAgentConfig.AfterModelCallbacks. So the interception happens one layer
// down, at the response. For a Task agent, a model response carrying no
// function calls is terminal: the flow loop returns, the iterator drains, and
// runChat sees out == nil. Appending a finish_task call to that response makes
// the base flow execute it — handleFunctionCalls reads the post-callback
// response, and finalizeModelResponseEvent stamps the call with an ID from the
// same generator a model-issued call gets — which produces exactly the event
// pair runTask waits for: the call sets pendingFCArgs, and the tool's success
// response promotes them to the node's Output.
func FinishOnStall(agentName string, payload StallPayload) llmagent.AfterModelCallback {
	if payload == nil {
		payload = DefaultStallPayload
	}
	return func(_ adkagent.Context, resp *model.LLMResponse, err error) (*model.LLMResponse, error) {
		if err != nil || resp == nil || resp.Content == nil {
			return nil, nil
		}
		// Partials aggregate into a final response this callback sees
		// separately; a fragment is not a finished turn. An error response is
		// not one either, and Interrupted means a long-running tool is waiting
		// on a human — that pause is legitimate and must survive.
		if resp.Partial || resp.Interrupted || resp.ErrorCode != "" {
			return nil, nil
		}
		if len(resp.Content.Parts) == 0 {
			return nil, nil
		}
		for _, p := range resp.Content.Parts {
			// Any function call at all means the turn continues: the flow will
			// run the tool and call the model again. That includes finish_task
			// itself, which is the ordinary path this must not disturb.
			if p != nil && p.FunctionCall != nil {
				return nil, nil
			}
		}

		// Parts are kept rather than replaced. Providers that sign thinking
		// blocks require them to be replayed intact, the text is worth having
		// in the agent's own history, and an assistant message carrying both
		// text and a tool call is legal.
		parts := make([]*genai.Part, 0, len(resp.Content.Parts)+1)
		parts = append(parts, resp.Content.Parts...)
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
			// ID is left empty on purpose: ADK stamps one in
			// finalizeModelResponseEvent.
			Name: FinishTaskToolName,
			Args: payload(agentName, responseText(resp.Content)),
		}})

		out := *resp
		content := *resp.Content
		content.Parts = parts
		out.Content = &content
		return &out, nil
	}
}

// responseText concatenates a content's text parts, skipping thinking blocks —
// they are for the provider's replay, not for the caller to read.
func responseText(c *genai.Content) string {
	var b strings.Builder
	for _, p := range c.Parts {
		if p == nil || p.Text == "" || p.Thought {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}
