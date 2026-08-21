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

// Operator acknowledgements — the durable half of the v0.5 W4.6 ack
// path (docs/v0.5-plan.md).
//
// # Why mast keeps a record of a suppression it does not own
//
// The suppression itself lives with whoever produced the finding: mast
// forwards the ack and the producer decides how long it lasts, because
// the producer is the only thing the next cycle's classification can be
// read from. What the producer does not have is the answer to "who
// silenced this, and when" — its ack surface takes an ack_by string
// from whoever calls it and has no way to check it. mast does have that
// answer, because the ack came through an authenticated ingress.
//
// So the split is: the producer is the store of record for THE
// SUPPRESSION, and mast is the store of record for WHO ASKED. Neither
// is a copy of the other, and mast's half is the half that cannot be
// reconstructed after the fact.
//
// # What is written, and what is not
//
// Two things land per ack. The append-only event on the ops row is the
// history — every ack, in order, with its attribution in the text — and
// it is what an audit reads. The per-subject state key is a snapshot of
// the most recent ack of that subject, which is what a re-ack reads to
// say "this was already acked by someone else an hour ago".
//
// Deliberately NOT written: a verdict, a grant, an expiry. An ack is
// not a mutation approval (docs/orchestration-design.md, "Ack routing"),
// so it does not go through pkg/approval, does not appear in the
// decision export, and carries no window of its own — mast recording an
// expiry it does not enforce would be a second, wrong answer to a
// question the producer already answers.
package transcript

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ackKeyPrefix keys one record per acked subject on the ack row.
const ackKeyPrefix = "mast_ack:"

// ackRowID is the pseudo-session whose companion ops row holds the ack
// records. Like [scheduleRowID] it has no primary row and never will:
// an ack is about a subject in the monitored world, not about any mast
// session, and there is frequently no session in flight when one
// arrives. List, ScanInterrupted and findUserID all skip rows ending in
// opsSuffix, so the record is invisible to `mast sessions list` and
// cannot be resumed, aborted or paused.
const ackRowID = "mast-acks"

// AckRecord is one operator acknowledgement as mast recorded it.
type AckRecord struct {
	// Workload names the bundle whose monitor.ack forwarded this.
	Workload string `json:"workload"`

	// Subject is the producer's subject key, verbatim. mast does not
	// parse it — see pkg/monitor.Transition.SubjectKey.
	Subject string `json:"subject"`

	// By is the authenticated caller, resolved from the credential the
	// request presented and never from its body. When a relay asserted
	// a human through the proxy path this is the human, and ProxyBy is
	// the relay: either alone answers the wrong question afterwards.
	By      string `json:"by"`
	ProxyBy string `json:"proxy_by,omitempty"`

	// Reason is the operator's note, if they left one.
	Reason string `json:"reason,omitempty"`

	// At is when mast accepted the ack (UTC).
	At time.Time `json:"at"`

	// Forwarded reports whether the producer's ack tool accepted it. A
	// false here with a record present is the interesting state: mast
	// has an attributed ack that the suppression never reached, which
	// is the difference between "nobody acked" and "the ack failed".
	Forwarded bool `json:"forwarded"`
}

// ackKey is the state key for one (workload, subject) pair.
//
// Digested rather than concatenated because a subject key is the
// producer's opaque string and may contain anything, including whatever
// separator the concatenation picked — two distinct subjects that
// collided across it would silently overwrite each other's attribution.
// The readable fields live inside the record, which is what anything
// reading these actually parses.
//
// Length-prefixed rather than delimited, for the same reason and one
// step further: a delimiter the subject can also contain only moves the
// ambiguity, since ("w", "a\x00b") and ("w\x00a", "b") hash the same.
// The length makes the encoding injective whatever either string holds.
func ackKey(workload, subject string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%s%s", len(workload), workload, subject))
	return ackKeyPrefix + hex.EncodeToString(sum[:12])
}

// RecordAck writes the durable ack record and appends its audit event.
//
// The write happens BEFORE the forward — see cmd/mast's acker — so the
// ordering question is what a failed forward leaves behind: an
// attributed ack marked Forwarded=false. That is the honest state and
// the recoverable one. The alternative, recording only what succeeded,
// loses the fact that a named operator asked at a named time, which is
// the one fact mast alone holds.
func (s *Store) RecordAck(ctx context.Context, userID string, rec AckRecord) error {
	if userID == "" {
		return fmt.Errorf("record ack for subject %q: userID is required (the ack row has no primary session to infer it from)", rec.Subject)
	}
	if strings.TrimSpace(rec.Workload) == "" || strings.TrimSpace(rec.Subject) == "" {
		return fmt.Errorf("record ack: workload and subject are both required (they are the record's key)")
	}
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	rec.At = rec.At.UTC()
	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal ack for subject %q: %w", rec.Subject, err)
	}
	text := fmt.Sprintf("%s acknowledged %q on workload %q at %s",
		rec.By, rec.Subject, rec.Workload, rec.At.Format(time.RFC3339Nano))
	if rec.ProxyBy != "" {
		text += " (asserted by " + rec.ProxyBy + ")"
	}
	if rec.Reason != "" {
		text += ": " + rec.Reason
	}
	return s.appendOpsDelta(ctx, userID, ackRowID, "ack", "operator", text,
		map[string]any{ackKey(rec.Workload, rec.Subject): string(blob)})
}

// Ack reads the most recent acknowledgement of one subject, or nil when
// mast has never recorded one.
//
// A missing row, an unreadable one and a corrupt record all read as "no
// ack", the same overlay semantics every other marker read here uses.
// The consequence is bounded: the caller loses the "previously acked
// by" line on its response and forwards the ack anyway. It is not a
// gate, and must not become one — whether a second ack is redundant is
// the producer's call, not mast's.
func (s *Store) Ack(ctx context.Context, userID, workload, subject string) *AckRecord {
	raw := s.opsState(ctx, userID, ackRowID)[ackKey(workload, subject)]
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var rec AckRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil
	}
	return &rec
}

// Acks returns every subject this workload has a recorded ack for, in
// no particular order. Snapshot semantics: one record per subject, the
// most recent. The full history is the ops row's event stream.
func (s *Store) Acks(ctx context.Context, userID, workload string) []AckRecord {
	var out []AckRecord
	for k, raw := range s.opsState(ctx, userID, ackRowID) {
		if !strings.HasPrefix(k, ackKeyPrefix) {
			continue
		}
		var rec AckRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			continue
		}
		if workload != "" && rec.Workload != workload {
			continue
		}
		out = append(out, rec)
	}
	return out
}
