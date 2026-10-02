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

package transcript

import (
	"context"
	"strings"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
)

// The durable half of the ack path (v0.5 W4.6). mast is the store of
// record for WHO ASKED; the producer owns the suppression itself.

// TestAckRoundTrip: what mast reads back is what it wrote, over both
// services — the one that matters in production is the durable one.
func TestAckRoundTrip(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			store := NewStore(svc, testApp)
			ctx := context.Background()
			at := time.Date(2026, 8, 21, 9, 15, 0, 0, time.UTC)

			if got := store.Ack(ctx, "u1", "gke-triage", "ns/checkout/oom"); got != nil {
				t.Fatalf("Ack on a fresh store = %+v, want nil", got)
			}
			rec := AckRecord{
				Workload:  "gke-triage",
				Subject:   "ns/checkout/oom",
				By:        "alice@example.com",
				ProxyBy:   "sa:switchboard",
				Reason:    "known, fix is in flight",
				At:        at,
				Forwarded: true,
			}
			if err := store.RecordAck(ctx, "u1", rec); err != nil {
				t.Fatalf("RecordAck: %v", err)
			}
			got := store.Ack(ctx, "u1", "gke-triage", "ns/checkout/oom")
			if got == nil {
				t.Fatal("Ack returned nil for a record just written")
			}
			// The attribution pair is the point of the record: either
			// half alone answers the wrong question afterwards.
			if got.By != rec.By || got.ProxyBy != rec.ProxyBy {
				t.Errorf("attribution = %q / %q, want %q / %q", got.By, got.ProxyBy, rec.By, rec.ProxyBy)
			}
			if !got.At.Equal(at) || got.Reason != rec.Reason || !got.Forwarded {
				t.Errorf("Ack = %+v, want %+v", got, rec)
			}

			// One record per subject, last write wins: a re-ack replaces
			// the snapshot, and the history lives in the event stream.
			rec.By = "bob@example.com"
			rec.At = at.Add(time.Hour)
			if err := store.RecordAck(ctx, "u1", rec); err != nil {
				t.Fatalf("RecordAck (re-ack): %v", err)
			}
			if got := store.Ack(ctx, "u1", "gke-triage", "ns/checkout/oom"); got.By != "bob@example.com" {
				t.Errorf("after a re-ack, Ack = %+v, want the second one", got)
			}

			// Another subject does not overwrite the first, and neither
			// does the same subject on another workload.
			for _, other := range []AckRecord{
				{Workload: "gke-triage", Subject: "ns/api/crashloop", By: "carol@example.com", At: at},
				{Workload: "cost-sweep", Subject: "ns/checkout/oom", By: "dave@example.com", At: at},
			} {
				if err := store.RecordAck(ctx, "u1", other); err != nil {
					t.Fatalf("RecordAck (%s/%s): %v", other.Workload, other.Subject, err)
				}
			}
			if got := store.Ack(ctx, "u1", "gke-triage", "ns/checkout/oom"); got.By != "bob@example.com" {
				t.Errorf("the first subject's ack changed: %+v", got)
			}
			if got := store.Ack(ctx, "u1", "cost-sweep", "ns/checkout/oom"); got == nil || got.By != "dave@example.com" {
				t.Errorf("the same subject on another workload = %+v, want its own record", got)
			}

			// Acks filters by workload, because an operator asking what
			// is muted is asking about one monitor.
			acks := store.Acks(ctx, "u1", "gke-triage")
			if len(acks) != 2 {
				t.Errorf("Acks(gke-triage) = %+v, want the two subjects acked on it", acks)
			}
			if all := store.Acks(ctx, "u1", ""); len(all) != 3 {
				t.Errorf("Acks(all) = %+v, want every recorded ack", all)
			}
		})
	}
}

// TestAckKeyDigestsTheSubject: a subject key is the producer's opaque
// string and may contain anything, delimiters included. Two subjects
// that differ only in where a separator falls must not collide — a
// collision here silently attributes one operator's ack to another's
// subject, which is the one failure this record exists to rule out.
func TestAckKeyDigestsTheSubject(t *testing.T) {
	a := ackKey("watch", "ns/checkout\x00oom")
	b := ackKey("watch\x00ns/checkout", "oom")
	if a == b {
		t.Errorf("ackKey collided across the workload/subject boundary: %q", a)
	}
	if !strings.HasPrefix(a, ackKeyPrefix) {
		t.Errorf("ackKey = %q, want the %q prefix so Acks can enumerate it", a, ackKeyPrefix)
	}
	if a != ackKey("watch", "ns/checkout\x00oom") {
		t.Error("ackKey is not stable; a re-ack would write a second record instead of replacing one")
	}
}

// TestAckWritesAnAuditEvent: the snapshot answers "is this muted"; the
// append-only event answers "who muted it, when, and why", which is what
// an incident review reads. The attribution has to be legible in the
// text, not only in the JSON.
func TestAckWritesAnAuditEvent(t *testing.T) {
	svc := services(t)["inmemory"]
	store := NewStore(svc, testApp)
	ctx := context.Background()

	if err := store.RecordAck(ctx, "u1", AckRecord{
		Workload: "gke-triage",
		Subject:  "ns/checkout/oom",
		By:       "alice@example.com",
		ProxyBy:  "sa:switchboard",
		Reason:   "known",
		At:       time.Date(2026, 8, 21, 9, 15, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("RecordAck: %v", err)
	}
	resp, err := svc.Get(ctx, &adksession.GetRequest{
		AppName: testApp, UserID: "u1", SessionID: opsSessionID(ackRowID),
	})
	if err != nil {
		t.Fatalf("Get ops row: %v", err)
	}
	var texts []string
	for ev := range resp.Session.Events().All() {
		if ev.Content != nil && len(ev.Content.Parts) > 0 {
			texts = append(texts, ev.Content.Parts[0].Text)
		}
	}
	if len(texts) != 1 {
		t.Fatalf("ops row has %d events, want one per ack", len(texts))
	}
	text := texts[0]
	for _, want := range []string{
		"alice@example.com", "ns/checkout/oom", "gke-triage",
		"2026-08-21T09:15:00Z", "asserted by sa:switchboard", "known",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("audit text %q does not mention %q", text, want)
		}
	}

	// A second ack appends rather than replaces: the history is the
	// history, including the acks that were later superseded.
	if err := store.RecordAck(ctx, "u1", AckRecord{
		Workload: "gke-triage", Subject: "ns/checkout/oom", By: "bob@example.com", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordAck (second): %v", err)
	}
	resp, err = svc.Get(ctx, &adksession.GetRequest{
		AppName: testApp, UserID: "u1", SessionID: opsSessionID(ackRowID),
	})
	if err != nil {
		t.Fatalf("Get ops row (second): %v", err)
	}
	var n int
	for range resp.Session.Events().All() {
		n++
	}
	if n != 2 {
		t.Errorf("ops row has %d events after a re-ack, want both", n)
	}
}

// TestAckIsNotASession: the ack row has no primary, so an operator
// listing sessions never sees suppression bookkeeping among their
// workload's runs — and nothing can resume, abort or pause it.
func TestAckIsNotASession(t *testing.T) {
	store := NewStore(services(t)["inmemory"], testApp)
	ctx := context.Background()

	if err := store.RecordAck(ctx, "u1", AckRecord{
		Workload: "gke-triage", Subject: "s", By: "alice@example.com", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordAck: %v", err)
	}
	list, err := store.List(ctx, "u1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("sessions list = %+v, want the ack row to be invisible", list)
	}
	if !IsReservedSessionID(opsSessionID(ackRowID)) {
		t.Error("the ack row is not in the reserved namespace; a real session could collide with it")
	}
	intr, err := store.ScanInterrupted(ctx)
	if err != nil {
		t.Fatalf("ScanInterrupted: %v", err)
	}
	if len(intr) != 0 {
		t.Errorf("ScanInterrupted = %+v, want the ack row skipped", intr)
	}
}

// TestRecordAckRefusesAnIncompleteRecord: the userID has no primary row
// to be inferred from, and the workload/subject pair IS the key. A
// caller that forgets one is told, not silently written under a guess.
func TestRecordAckRefusesAnIncompleteRecord(t *testing.T) {
	store := NewStore(services(t)["inmemory"], testApp)
	ctx := context.Background()
	full := AckRecord{Workload: "w", Subject: "s", By: "alice@example.com"}

	for _, tc := range []struct {
		name   string
		userID string
		rec    AckRecord
	}{
		{"no userID", "", full},
		{"no workload", "u1", AckRecord{Subject: "s", By: "alice@example.com"}},
		{"blank workload", "u1", AckRecord{Workload: "  ", Subject: "s", By: "alice@example.com"}},
		{"no subject", "u1", AckRecord{Workload: "w", By: "alice@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.RecordAck(ctx, tc.userID, tc.rec); err == nil {
				t.Errorf("RecordAck accepted a record with %s", tc.name)
			}
		})
	}
}

// TestAckUnreadableRecordsReadAsAbsent: a corrupt record costs the
// caller its "previously acked by" line and nothing else. This read must
// never become a gate — whether a second ack is redundant is the
// producer's call, so failing open here is failing towards forwarding.
func TestAckUnreadableRecordsReadAsAbsent(t *testing.T) {
	store := NewStore(services(t)["inmemory"], testApp)
	ctx := context.Background()

	for _, tc := range []struct{ name, blob string }{
		{"not JSON", "{{"},
		{"blanked", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.appendOpsDelta(ctx, "u1", ackRowID, "ack", "operator", "corrupt",
				map[string]any{ackKey("w", "s"): tc.blob}); err != nil {
				t.Fatalf("appendOpsDelta: %v", err)
			}
			if got := store.Ack(ctx, "u1", "w", "s"); got != nil {
				t.Errorf("Ack = %+v, want nil for %s", got, tc.name)
			}
			if got := store.Acks(ctx, "u1", "w"); len(got) != 0 {
				t.Errorf("Acks = %+v, want the unreadable record skipped rather than counted", got)
			}
		})
	}
}

// TestRecordAckStampsATimestamp: every ack answers "when", so a caller
// that leaves At zero gets the write time rather than the zero year.
func TestRecordAckStampsATimestamp(t *testing.T) {
	store := NewStore(services(t)["inmemory"], testApp)
	ctx := context.Background()
	before := time.Now().UTC().Add(-time.Second)

	if err := store.RecordAck(ctx, "u1", AckRecord{
		Workload: "w", Subject: "s", By: "alice@example.com",
	}); err != nil {
		t.Fatalf("RecordAck: %v", err)
	}
	got := store.Ack(ctx, "u1", "w", "s")
	if got == nil || got.At.Before(before) {
		t.Errorf("Ack = %+v, want a stamped timestamp", got)
	}
}
