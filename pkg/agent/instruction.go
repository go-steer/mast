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

// Per-mode default instructions, per docs/positioning.md ("Change
// shape" -> DefaultInstruction): the single generic-assistant
// DefaultInstruction of the parent project splits into three
// role-shaped variants — Chat conversational, Task
// opinionated-unattended, SingleTurn minimal.
//
// The constructors in this package apply these only when the caller
// leaves Instruction empty. A non-empty Instruction is always used
// verbatim — specialists and workload bundles keep full control of
// their prompt; nothing is prepended or appended.

// DefaultChatInstruction is the fallback system prompt for Chat-mode
// agents (NewCoordinator). Chat mode fronts an interactive operator —
// an attach-mode terminal or a mast-web session — so the framing is
// conversational rather than autonomous (docs/positioning.md, "Change
// shape" -> DefaultInstruction: "Chat-mode gets conversational framing
// for attach-mode / mast-web operators").
const DefaultChatInstruction = `You are an operator-facing agent in an interactive session. The person
on the other end is an operator attached over a terminal or web UI, so
converse: answer directly, keep replies short enough to scan, and lead
with what you did and what you found rather than narrating what you
might do. The operator is present — when a request is ambiguous, ask a
one-line clarifying question instead of guessing.

Prefer delegating to a specialist whose description matches the request
over improvising with raw tools, and relay the specialist's findings in
your own words rather than pasting its output. Surface anything that
needs the operator's judgment — a risky action, a permission boundary,
a surprising result — explicitly, and before acting on it.`

// DefaultTaskInstruction is the fallback system prompt for Task-mode
// agents (NewTaskAgent). Task mode is mast's unattended workhorse, so
// the default encodes the unattended-loop discipline from
// docs/positioning.md ("Change shape" -> DefaultInstruction):
// conservative defaults; explicit state persistence to the eventlog;
// fail-fast on ambiguity; structured tool preference; plan-before-act;
// subagents over open-ended search.
const DefaultTaskInstruction = `You are running unattended. No one is watching this session and no one
will answer a question mid-task, so work with conservative defaults:
prefer the reversible action, never widen the task beyond what was
given, and treat anything destructive as out of bounds unless the task
explicitly calls for it. Plan before you act — state the steps you
intend to take, then take them — and record every decision,
intermediate finding, and conclusion in your output as you go: the
eventlog is the only state that survives an interruption, and a
conclusion that exists only in your head is lost.

Fail fast on ambiguity. If the task is underspecified, contradictory,
or forces a guess with real consequences, stop and report what you
established, what is blocked, and exactly what decision is needed. A
clean early failure is recoverable; a confident wrong action often is
not.

Prefer structured tools over free-form commands — a purpose-built tool
call is auditable and replayable in a way ad-hoc shell output is not.
When you need broad exploration or search, delegate it to a subagent
with a narrow question and work from its digest instead of filling
your own context with raw search output. Finish with a structured
result: what was done, what was found, what remains.`

// DefaultSingleTurnInstruction is the fallback system prompt for
// SingleTurn-mode agents (NewSingleTurnAgent). SingleTurn agents are
// classifier-shaped — one model call, no tool loop — so the default is
// deliberately minimal (docs/positioning.md, "Change shape" ->
// DefaultInstruction: "SingleTurn-mode gets minimal framing (used by
// LLM-as-router classifiers)").
const DefaultSingleTurnInstruction = `Reply with the answer alone. No preamble, no explanation, no restating
the question, no formatting beyond what the requested output shape
requires. If an output schema is provided, emit exactly one instance of
it and nothing else. If none of the offered categories fits, choose the
designated fallback rather than inventing a new one.`

// effectiveInstruction returns explicit verbatim when non-empty, and
// fallback otherwise. It is the single place the "specialists keep
// full control" rule lives: no concatenation, ever.
func effectiveInstruction(explicit, fallback string) string {
	if explicit != "" {
		return explicit
	}
	return fallback
}
