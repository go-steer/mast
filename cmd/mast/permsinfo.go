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

package main

import (
	"context"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/attach"
)

// permsReadTimeout bounds the decision-log read behind GET /perms.
// Same budget and same posture as turnStateReadTimeout: this is an
// operator surface reached precisely when something has gone wrong, so
// a wedged database costs the approvals list and not the request.
const permsReadTimeout = turnStateReadTimeout

// perms projects what governs one session onto the attach wire: the
// permissions gate's configured policy, the workload's write-gate
// policy, and the approvals already adjudicated in this session.
//
// Answers #375, in which this route returned 200 with the zero
// PermsInfo on every mast daemon ever shipped. Two things made that
// worse than a missing feature. The zero value is not visibly empty —
// it is a well-formed description of a daemon that gates nothing,
// which is the opposite of the truth for the require_approval default.
// And a mast daemon has more to say here than the core-agent shape
// this route was ported from: its adjudications are durable, and until
// now no HTTP surface exposed them at all — only `mast sessions
// export-decisions`, which is not reachable from a UI.
//
// The read is per session because the decision log is. The gate is
// not: it is daemon-wide, so two sessions report the same mode and
// different approvals, which is accurate.
func (w attachWiring) perms(sid string) attach.PermsInfo {
	out := attach.PermsInfo{OnMutation: w.onMutation}
	// Mode, Allow and Deny come from the gate or not at all. Under
	// apply or dry_run compose builds no gate, and inventing a mode
	// string for that case would be #133's defect in a new place: a
	// field whose value is a plausible answer to a question nothing
	// asked.
	if w.gate != nil {
		snap := w.gate.Snapshot()
		out.Mode = string(snap.Mode)
		out.Allow = snap.Allow
		out.Deny = snap.Deny
	}
	out.Approvals = w.approvals(sid)
	return out
}

// approvals reads this session's durable adjudications and maps them
// onto the wire's approval-log rows.
//
// Deliberately NOT gate.Approvals(): that log is in-memory, lives for
// the daemon's lifetime, and carries no session id, so on a
// multi-session daemon it would attribute every session's answers to
// whichever one was asked. The durable log is the one that is right,
// and it is also the one that survives the restart an operator is
// usually investigating.
//
// Every failure reports no rows rather than failing the request, for
// the same reason turnState does: the rules half of the answer is
// still true and still worth serving.
func (w attachWiring) approvals(sid string) []attach.ApprovalInfo {
	if w.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(w.contextOrBackground(), permsReadTimeout)
	defer cancel()
	// Empty userID: the store scopes by session, and the daemon's own
	// user is the only one writing decisions for a session it runs.
	decisions, err := w.store.Decisions(ctx, "", sid)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("perms: could not read the session's decisions",
				"session", sid, "error", err.Error())
		}
		return nil
	}
	if len(decisions) == 0 {
		return nil
	}
	out := make([]attach.ApprovalInfo, 0, len(decisions))
	for _, d := range decisions {
		out = append(out, approvalInfo(d))
	}
	return out
}

// approvalInfo maps one durable decision onto the wire row.
//
// The interesting choice is which field drives Decision. It is
// Disposition, not Outcome. Outcome is what the operator answered and
// it has three values mast's own vocabulary needs but this wire
// contract does not carry; worse, it is empty on a call that fired
// under an earlier change-set grant, and it can say "approve" about a
// call mast then refused. Disposition is what the gate DID, it is
// total over every record, and it is the only one of the two that a
// client rendering "was this allowed?" can trust.
//
// What that flattening loses is recovered by Refusal beside it: a deny
// row keeps the distinction between a person saying no and mast
// refusing the person's answer, which the three-value permissions
// vocabulary has no spelling for.
func approvalInfo(d approval.Decision) attach.ApprovalInfo {
	info := attach.ApprovalInfo{
		Tool:      d.Tool,
		Key:       d.ProposedKey,
		At:        d.DecidedAt,
		Approver:  d.Approver,
		Refusal:   d.Refusal,
		ChangeSet: d.ChangeSet,
	}
	// On an edit the proposal is not what ran, and the row describes
	// what ran. The proposed→executed pair stays available in full via
	// `mast sessions export-decisions`; compressing it into one wire
	// field here would make a client render the arguments the operator
	// specifically rejected.
	if d.Edited() {
		info.Key = d.ExecutedKey
	}
	switch d.Disposition {
	case approval.DispositionAuthorized:
		// allow-once, never allow-session: mast adjudicates one call at
		// a time and holds no session-scoped allow. A change-set grant
		// is the nearest thing to one and is reported by ChangeSet
		// above, on the row it actually authorized.
		info.Decision = "allow-once"
	case approval.DispositionRefusedByOperator, approval.DispositionRefusedByMast:
		info.Decision = "deny"
	default:
		// A record written by a future disposition this build does not
		// know. Leave Decision empty rather than guessing: an unknown
		// answer rendered as "allow-once" is the failure mode this
		// whole issue is about.
		info.Decision = ""
	}
	return info
}
