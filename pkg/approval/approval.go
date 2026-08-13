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

// Package approval is the write gate: the seam where a mutating tool
// call stops and waits for an operator (docs/v0.3-plan.md W2,
// hitl_policy.on_mutation).
//
// It is deliberately two halves that meet here and nowhere else:
//
//   - pkg/permissions decides POLICY — may this call proceed without
//     asking, must it ask, or is it refused outright. That package is
//     ADK-independent and stays that way.
//   - ADK's tool-confirmation flow performs the PAUSE. It is durable:
//     the request is a function call in the session event log, so the
//     turn survives a process death and resumes from the log. mast's
//     own permissions.Prompter is a synchronous in-process ask and by
//     construction cannot survive a restart (scoreboard row 5), which
//     is why the pause is not built on it.
//
// The seam itself is an ADK runner plugin's BeforeToolCallback, which
// is the only place that sees every tool call — builtin, MCP, or
// specialist-scoped — before it runs. Registration order matters and is
// settled: the pkg/effects outbox plugin goes first, this one second, so
// a replayed call is answered from the outbox without asking an operator
// to approve a mutation that already happened (resolved-decision 144).
//
// The substrate facts this package is built on are pinned by
// adkseam_test.go rather than assumed; each is load-bearing and none is
// documented by ADK.
package approval
