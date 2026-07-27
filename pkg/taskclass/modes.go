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

package taskclass

// This file is the mast-side task-class integration (not ported from
// the parent project): the public-class → ADK-mode mapping and the
// per-class instruction defaults from docs/orchestration-design.md
// "Task-class resolution":
//
//   - chat  → Chat mode (interactive coordinator role)
//   - debug / implement / research / review → Task mode with a
//     per-class DefaultInstruction variant
//   - orchestrate → Task mode with the planner enabled (the root is
//     the planner scaffold from pkg/planner, whose own instruction
//     template applies — see PlannerEnabled)
//
// SingleTurn is deliberately absent: it is an internal mode (specialist
// frontmatter `mode: SingleTurn`, LLM-as-router classifiers), never a
// public task class (orchestration-design, "SingleTurn is internal").
//
// # Instruction precedence
//
// Three layers compose, most specific wins:
//
//  1. An explicit caller/spec Instruction (workload bundle, specialist
//     frontmatter, library caller) is always used verbatim — nothing
//     is prepended or appended (pkg/agent's "specialists keep full
//     control" rule).
//  2. Otherwise the class profile's instruction default (Instruction
//     below) applies: an explicit class profile beats the generic
//     mode default.
//  3. Otherwise — no class declared, or a class with no per-class
//     text — pkg/agent's per-mode fallback applies
//     (DefaultChatInstruction / DefaultTaskInstruction /
//     DefaultSingleTurnInstruction), enforced by the pkg/agent
//     constructors when they see an empty Instruction.
//
// In practice a caller resolves `instr := explicit; if instr == "" {
// instr = taskclass.Instruction(class) }` and hands the result to the
// pkg/agent constructor, whose empty-string fallback supplies layer 3.

// Agent-mode labels returned by AgentMode. String-typed (not ADK
// types) so taskclass stays a pure-data package — the ADK wiring
// happens in the caller (internal/compose, cmd/mast).
const (
	ModeChat = "Chat"
	ModeTask = "Task"
)

// AgentMode returns the ADK agent mode a public task class runs
// under, or "" for an unknown/empty class (caller should surface an
// error listing Classes rather than guess).
func AgentMode(class string) string {
	switch class {
	case Chat:
		return ModeChat
	case Debug, Implement, Research, Review, Orchestrate:
		return ModeTask
	}
	return ""
}

// PlannerEnabled reports whether the class runs with the planner as
// the root shape (docs/orchestration-design.md: bundles that enable
// the planner typically declare task_class: orchestrate).
func PlannerEnabled(class string) bool { return class == Orchestrate }

// Instruction returns the per-class instruction default, or "" when
// the class has none (unknown classes, and classes whose right
// default is the generic per-mode fallback). See the precedence note
// in this file's doc comment: callers pass a non-empty result to the
// pkg/agent constructor so the class profile beats the generic mode
// default, and pass "" through so the constructor's own fallback
// applies.
//
// chat and orchestrate intentionally return "": chat's right framing
// IS pkg/agent's DefaultChatInstruction, and orchestrate's root is
// the planner, whose template (pkg/planner DefaultInstructionTemplate)
// embeds the orchestration frame itself.
func Instruction(class string) string {
	switch class {
	case Debug:
		return debugInstruction
	case Implement:
		return implementInstruction
	case Research:
		return researchInstruction
	case Review:
		return reviewInstruction
	}
	return ""
}

// Per-class instruction defaults. Each variant embeds the unattended
// Task-mode discipline (conservative defaults, fail-fast on ambiguity,
// record findings as you go — the same spine as pkg/agent's
// DefaultTaskInstruction) and then sharpens it for the class's job.
// Kept as unexported consts; Instruction is the lookup surface.
const (
	debugInstruction = `You are running unattended on a debugging task. Work from evidence:
reproduce or observe the failure before hypothesizing, state each
hypothesis and the check that would falsify it, and record every
finding — a symptom, a ruled-out cause, a suspicious log line — in
your output as you establish it. Prefer reversible, read-only
diagnostics; treat fixes as out of bounds unless the task explicitly
asks for one, and never widen the investigation beyond the failure
you were given. If the evidence runs out or the reproduction is
ambiguous, stop and report what you established, what you ruled out,
and exactly what is still unknown. Finish with a structured verdict:
root cause (or candidates ranked by likelihood), the evidence for it,
and the minimal fix or next diagnostic you recommend.`

	implementInstruction = `You are running unattended on an implementation task. Plan before you
act: state the steps you intend to take, then take them, recording
each decision and its rationale in your output as you go. Follow the
conventions the surrounding code already uses; make the smallest
change that completes the task, and never widen scope beyond what was
given. Verify as you go — build, run the relevant tests, and treat a
red result as part of the task, not someone else's problem. Fail fast
on ambiguity: if the task is underspecified or forces a consequential
guess, stop and report what is blocked and what decision is needed.
Finish with a structured result: what changed, how it was verified,
and anything left undone.`

	researchInstruction = `You are running unattended on a research task. Your job is to gather
and synthesize, not to modify: treat every action as read-only, and
delegate broad sweeps to subagents with narrow questions, working
from their digests instead of raw output. Track provenance — every
claim in your findings should name the file, document, or observation
it came from — and separate what the evidence shows from what you
infer. Cover the question's full breadth before going deep on any one
branch, and record findings incrementally as you establish them. If
sources conflict, report the conflict rather than silently picking a
side. Finish with a structured synthesis: findings with provenance,
open questions, and confidence.`

	reviewInstruction = `You are running unattended on a review task. Read the change in full
before judging any part of it, and evaluate what is actually there —
correctness, edge cases, security exposure, consistency with the
surrounding code — rather than restyling it to taste. Every finding
must cite the specific location and explain the concrete consequence;
rank findings by severity, and say plainly when a concern is
speculative. The change is not yours to fix: propose, don't rewrite.
If the change's intent is ambiguous, review what it does and flag the
ambiguity rather than guessing the intent. Finish with a structured
verdict: findings by severity with locations, what was checked and
found sound, and an overall recommendation.`
)
