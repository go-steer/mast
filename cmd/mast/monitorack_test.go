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
	"errors"
	"strings"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/monitor"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// The ack leg (v0.5 W4.6): attribute, record, forward — in that order.
// pkg/inject's tests pin where the identity comes from; these pin what
// mast does with it.

func testAcker(t *testing.T, rec *recordingRun, ack *workload.MonitorAck) (*monitorAcker, *transcript.Store) {
	t.Helper()
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	a := newMonitorAcker(discardLogger(), primedObs(), store,
		workload.Monitor{Ack: ack}, rec.run, "gke-triage", appName, "daemon")
	a.now = func() time.Time { return time.Date(2026, 8, 21, 9, 15, 0, 0, time.UTC) }
	return a, store
}

func ackCtx(identity string) context.Context {
	return auth.WithCaller(context.Background(), auth.Caller{Identity: identity})
}

// TestAckForwardsWithTheCallersIdentity: the two arguments mast supplies
// are the two an operator must not be able to set. Everything else on
// the call is the bundle's.
func TestAckForwardsWithTheCallersIdentity(t *testing.T) {
	rec := &recordingRun{}
	a, store := testAcker(t, rec, &workload.MonitorAck{
		Tool: "findings_ack",
		Args: map[string]any{"window": "4h", "cluster": "prod"},
	})

	res, err := a.forward(ackCtx("alice@example.com"), inject.MonitorAckRequest{
		Subject: "ns/checkout/oom",
		Reason:  "known",
	})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if len(rec.saw) != 1 || rec.saw[0] != "findings_ack" {
		t.Fatalf("ran %v, want the bundle's ack tool once", rec.saw)
	}
	args := rec.sawArgs[0]
	if args[monitor.AckSubjectArg] != "ns/checkout/oom" {
		t.Errorf("%s = %v, want the subject from the request", monitor.AckSubjectArg, args[monitor.AckSubjectArg])
	}
	if args[monitor.AckByArg] != "alice@example.com" {
		t.Errorf("%s = %v, want the authenticated caller", monitor.AckByArg, args[monitor.AckByArg])
	}
	// The deployment's own arguments still ride along: which cluster,
	// how long a suppression lasts.
	if args["window"] != "4h" || args["cluster"] != "prod" {
		t.Errorf("args = %v, want the bundle's literals carried through", args)
	}
	if res.AckBy != "alice@example.com" || res.Workload != "gke-triage" {
		t.Errorf("result = %+v, want the resolved identity and this daemon's workload", res)
	}

	// And the durable half: mast is the store of record for who asked.
	got := store.Ack(context.Background(), "daemon", "gke-triage", "ns/checkout/oom")
	if got == nil {
		t.Fatal("nothing recorded; the suppression happened with no trace of who asked")
	}
	if got.By != "alice@example.com" || got.Reason != "known" || !got.Forwarded {
		t.Errorf("record = %+v, want an attributed, forwarded ack", got)
	}
}

// TestAckArgsCannotBeOverriddenByTheBundle: the loader refuses a bundle
// that pins either argument, and this is the belt to that braces. An
// operator identity a YAML file could override is not an identity.
func TestAckArgsCannotBeOverriddenByTheBundle(t *testing.T) {
	rec := &recordingRun{}
	a, _ := testAcker(t, rec, &workload.MonitorAck{
		Tool: "findings_ack",
		Args: map[string]any{
			monitor.AckByArg:      "nobody",
			monitor.AckSubjectArg: "everything",
		},
	})

	if _, err := a.forward(ackCtx("alice@example.com"), inject.MonitorAckRequest{Subject: "s"}); err != nil {
		t.Fatalf("forward: %v", err)
	}
	args := rec.sawArgs[0]
	if args[monitor.AckByArg] != "alice@example.com" || args[monitor.AckSubjectArg] != "s" {
		t.Errorf("args = %v, want mast's two to win over the bundle's", args)
	}
}

// TestAckRecordsTheProxyAlongsideTheHuman: the in-chat path. Either half
// alone answers the wrong question afterwards — the human is who
// decided, the relay is how it arrived.
func TestAckRecordsTheProxyAlongsideTheHuman(t *testing.T) {
	rec := &recordingRun{}
	a, store := testAcker(t, rec, &workload.MonitorAck{Tool: "findings_ack"})
	ctx := auth.WithProxyBy(ackCtx("alice@example.com"), "sa:switchboard")

	if _, err := a.forward(ctx, inject.MonitorAckRequest{Subject: "s"}); err != nil {
		t.Fatalf("forward: %v", err)
	}
	got := store.Ack(context.Background(), "daemon", "gke-triage", "s")
	if got == nil || got.By != "alice@example.com" || got.ProxyBy != "sa:switchboard" {
		t.Fatalf("record = %+v, want both the human and the relay", got)
	}
	// But only the effective identity is forwarded: the producer's
	// suppression is the human's, and mast keeps the routing detail.
	if rec.sawArgs[0][monitor.AckByArg] != "alice@example.com" {
		t.Errorf("forwarded %v, want the human", rec.sawArgs[0][monitor.AckByArg])
	}
}

// TestAckRefusesAnUnattributedRequest: unreachable from the route, which
// attributes at least the shared credential, and refused rather than
// defaulted anyway. An ack whose whole purpose is to be attributable
// must not fall back to a mechanism name.
func TestAckRefusesAnUnattributedRequest(t *testing.T) {
	rec := &recordingRun{}
	a, _ := testAcker(t, rec, &workload.MonitorAck{Tool: "findings_ack"})

	_, err := a.forward(context.Background(), inject.MonitorAckRequest{Subject: "s"})
	if err == nil {
		t.Fatal("an unattributed ack was forwarded")
	}
	if !errors.Is(err, inject.ErrBadPayload) {
		t.Errorf("error = %v, want it to map to a 400 the caller can act on", err)
	}
	if len(rec.saw) != 0 {
		t.Errorf("ran %v; the suppression took with nobody's name on it", rec.saw)
	}
}

// TestAckRefusesAnotherWorkload: a relay with several mast deployments
// configured should fail loudly on the wrong one rather than suppress a
// finding on a cluster nobody was looking at.
func TestAckRefusesAnotherWorkload(t *testing.T) {
	rec := &recordingRun{}
	a, _ := testAcker(t, rec, &workload.MonitorAck{Tool: "findings_ack"})

	_, err := a.forward(ackCtx("alice@example.com"), inject.MonitorAckRequest{
		Subject: "s", Workload: "cost-sweep",
	})
	if err == nil {
		t.Fatal("an ack addressed to another workload was accepted")
	}
	if !strings.Contains(err.Error(), "gke-triage") || !strings.Contains(err.Error(), "cost-sweep") {
		t.Errorf("error = %v, want it to name both workloads", err)
	}
	if len(rec.saw) != 0 {
		t.Errorf("ran %v for another workload's subject", rec.saw)
	}
}

// TestAckKeepsTheRecordWhenTheForwardFails: the interesting state. mast
// has an attributed ack the suppression never reached, which is the
// difference between "nobody acked" and "the ack failed" — and the
// operator is told, so asking again is the recovery.
func TestAckKeepsTheRecordWhenTheForwardFails(t *testing.T) {
	rec := &recordingRun{fail: map[string]error{"findings_ack": errors.New("producer down")}}
	a, store := testAcker(t, rec, &workload.MonitorAck{Tool: "findings_ack"})

	if _, err := a.forward(ackCtx("alice@example.com"), inject.MonitorAckRequest{Subject: "s"}); err == nil {
		t.Fatal("forward reported success for an ack the producer refused")
	}
	got := store.Ack(context.Background(), "daemon", "gke-triage", "s")
	if got == nil {
		t.Fatal("no record at all; a failed forward erased the fact that a named operator asked")
	}
	if got.Forwarded {
		t.Error("record reads Forwarded, but the producer never took it")
	}
}

// TestAckReportsThePreviousAcker: a repeat ack is forwarded regardless —
// whether a second one is redundant is the producer's call — but the
// answer says who already asked. This is the reader that makes the
// durable record more than write-only.
func TestAckReportsThePreviousAcker(t *testing.T) {
	rec := &recordingRun{}
	a, _ := testAcker(t, rec, &workload.MonitorAck{Tool: "findings_ack"})

	if _, err := a.forward(ackCtx("alice@example.com"), inject.MonitorAckRequest{Subject: "s"}); err != nil {
		t.Fatalf("first forward: %v", err)
	}
	res, err := a.forward(ackCtx("bob@example.com"), inject.MonitorAckRequest{Subject: "s"})
	if err != nil {
		t.Fatalf("second forward: %v", err)
	}
	if res.PreviouslyAckedBy != "alice@example.com" {
		t.Errorf("PreviouslyAckedBy = %q, want alice", res.PreviouslyAckedBy)
	}
	if res.PreviouslyAckedAt == "" {
		t.Error("PreviouslyAckedAt is empty; \"already acked\" with no when is not much of an answer")
	}
	if len(rec.saw) != 2 {
		t.Errorf("ran %v, want the repeat forwarded too — suppression policy is the producer's", rec.saw)
	}
}

// TestAckCountsItsOutcomes: two outcomes and no more. A monitor whose
// acks are erroring is a monitor an operator believes they have silenced
// and have not.
func TestAckCountsItsOutcomes(t *testing.T) {
	rec := &recordingRun{fail: map[string]error{"findings_ack": errors.New("producer down")}}
	obs := primedObs()
	store := transcript.NewStore(adksession.InMemoryService(), appName)
	a := newMonitorAcker(discardLogger(), obs, store,
		workload.Monitor{Ack: &workload.MonitorAck{Tool: "findings_ack"}}, rec.run, "gke-triage", appName, "daemon")

	if _, err := a.forward(ackCtx("alice@example.com"), inject.MonitorAckRequest{Subject: "s"}); err == nil {
		t.Fatal("forward succeeded against a failing producer")
	}
	rec.fail = nil
	if _, err := a.forward(ackCtx("alice@example.com"), inject.MonitorAckRequest{Subject: "s"}); err != nil {
		t.Fatalf("forward: %v", err)
	}
	assertMetric(t, obs, `mast_monitor_acks_total{outcome="error",workload="gke-triage"} 1`)
	assertMetric(t, obs, `mast_monitor_acks_total{outcome="forwarded",workload="gke-triage"} 1`)
}

// TestAckDisabledWithoutABundleBlock: a workload that declares no
// monitor.ack takes no acks, and the route it backs answers 404 rather
// than pretending to have suppressed something.
func TestAckDisabledWithoutABundleBlock(t *testing.T) {
	rec := &recordingRun{}
	a, _ := testAcker(t, rec, nil)
	if a.enabled() {
		t.Fatal("enabled() = true for a workload with no monitor.ack block")
	}
	if _, err := a.forward(ackCtx("alice@example.com"), inject.MonitorAckRequest{Subject: "s"}); err == nil {
		t.Error("forward accepted an ack for a workload with nowhere to send it")
	}
}

// TestAckContextCarriesNoSession: an ack arrives when an operator reads
// their chat, which is rarely the moment a fire is running. The tool
// runs on the collection leg's context — no session, no invocation, no
// model — and must not be filed against whatever ran last.
func TestAckContextCarriesNoSession(t *testing.T) {
	rec := &recordingRun{}
	a, _ := testAcker(t, rec, &workload.MonitorAck{Tool: "findings_ack"})

	if _, err := a.forward(ackCtx("alice@example.com"), inject.MonitorAckRequest{Subject: "s"}); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if got := rec.sawCtx[0].SessionID(); got != "" {
		t.Errorf("ack ran under session %q, want none", got)
	}
	if got := rec.sawCtx[0].InvocationID(); got != "" {
		t.Errorf("ack ran under invocation %q, want none", got)
	}
}
