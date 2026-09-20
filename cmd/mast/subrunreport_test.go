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
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/planner"
)

// reportingObserver builds a sub-run observer wired to a real registry
// and a logger whose output the test can read.
func reportingObserver(t *testing.T) (*daemonSubRunObserver, *observability.Registry, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	obs := observability.New()
	// As main.go does at startup, so the "and not the other label"
	// assertions below read a published zero rather than an absence.
	obs.Prime("triage")
	sub := &daemonSubRunObserver{}
	sub.attach(newMeterPool(nil, nil, "", "echo"), obs, nil, nil, "triage",
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return sub, obs, &buf
}

// TestALostDispatchIsCountedAndNamed is the daemon half of #452. A
// provider rejection inside invoke_specialist is handed to the planner
// as an ordinary tool result, so the run finishes, the turn succeeds,
// and — before this change — nothing in the process recorded that a
// delegation had been lost. The planner is then free to do the
// specialist's work itself at full parent-context cost, on a run that
// still looks clean from the outside.
//
// Two reports, because neither is sufficient alone: the counter is what
// an alert fires on and cannot say which specialist, the log line says
// which and cannot be aggregated.
func TestALostDispatchIsCountedAndNamed(t *testing.T) {
	sub, obs, logs := reportingObserver(t)

	sink := sub.SubRun("incident-abc", "OOMKilled")
	sink.Close(planner.DispatchOutcome{
		Err: errors.New("googleapi: Error 429: Resource has been exhausted (e.g. check quota)., RESOURCE_EXHAUSTED"),
	})

	if got := scrapeCounter(t, obs, `mast_dispatches_total{outcome="rate_limited",workload="triage"}`); got != "1" {
		t.Errorf("rate_limited dispatches = %s, want 1", got)
	}
	// Split out of "failed" on purpose: a rate limit is the one dispatch
	// failure that is both transient and the operator's business, and an
	// alert that cannot tell it from a broken specialist is an alert
	// nobody can act on.
	if got := scrapeCounter(t, obs, `mast_dispatches_total{outcome="failed",workload="triage"}`); got != "0" {
		t.Errorf("a rate-limited dispatch was also counted as failed (%s); the two labels must be exclusive", got)
	}

	line := logs.String()
	for _, want := range []string{"OOMKilled", "incident-abc", "rate_limited", "RESOURCE_EXHAUSTED"} {
		if !strings.Contains(line, want) {
			t.Errorf("the log line does not mention %q; an operator cannot act on it:\n%s", want, line)
		}
	}
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("a lost delegation was not logged at WARN:\n%s", line)
	}
}

// TestADispatchFailureThatIsNotARateLimitIsCountedApart keeps the split
// honest in the other direction: if everything landed in rate_limited
// the label would carry no information.
func TestADispatchFailureThatIsNotARateLimitIsCountedApart(t *testing.T) {
	sub, obs, logs := reportingObserver(t)

	sink := sub.SubRun("incident-abc", "OOMKilled")
	sink.Close(planner.DispatchOutcome{Err: errors.New("specialist agent panicked mid-run")})

	if got := scrapeCounter(t, obs, `mast_dispatches_total{outcome="failed",workload="triage"}`); got != "1" {
		t.Errorf("failed dispatches = %s, want 1", got)
	}
	if got := scrapeCounter(t, obs, `mast_dispatches_total{outcome="rate_limited",workload="triage"}`); got != "0" {
		t.Errorf("a plain failure was counted as rate_limited (%s)", got)
	}
	if !strings.Contains(logs.String(), "specialist agent panicked mid-run") {
		t.Errorf("the failure's own text is missing from the log line:\n%s", logs.String())
	}
}

// TestAnOrdinaryDispatchIsCountedAndSaysNothing. The counter has to
// carry the denominator or "three rate_limited dispatches" is a number
// with no scale; the log must stay quiet, because a WARN per successful
// delegation is a WARN nobody reads.
func TestAnOrdinaryDispatchIsCountedAndSaysNothing(t *testing.T) {
	sub, obs, logs := reportingObserver(t)

	sink := sub.SubRun("incident-abc", "OOMKilled")
	sink.Close(planner.DispatchOutcome{})

	if got := scrapeCounter(t, obs, `mast_dispatches_total{outcome="ok",workload="triage"}`); got != "1" {
		t.Errorf("ok dispatches = %s, want 1", got)
	}
	if logs.Len() != 0 {
		t.Errorf("a dispatch that worked logged something:\n%s", logs.String())
	}
}

// TestAHaltedDispatchIsCountedAndNotLoggedTwice. The consumer that
// halted the dispatch — a budget ceiling, a watchdog trip — already
// logged it with the reason, and that line says strictly more than a
// generic one here would.
func TestAHaltedDispatchIsCountedAndNotLoggedTwice(t *testing.T) {
	sub, obs, logs := reportingObserver(t)

	sink := sub.SubRun("incident-abc", "OOMKilled")
	sink.Close(planner.DispatchOutcome{Halted: errors.New("budget: specialist OOMKilled exceeded $0.25")})

	if got := scrapeCounter(t, obs, `mast_dispatches_total{outcome="halted",workload="triage"}`); got != "1" {
		t.Errorf("halted dispatches = %s, want 1", got)
	}
	if got := scrapeCounter(t, obs, `mast_dispatches_total{outcome="failed",workload="triage"}`); got != "0" {
		t.Errorf("a halt was counted as a failure (%s); a ceiling firing is the system working", got)
	}
	if logs.Len() != 0 {
		t.Errorf("the halt was logged a second time here, adding nothing:\n%s", logs.String())
	}
}

// TestEveryDispatchOutcomeIsPublishedBeforeItHappens. An alert on
// rate_limited dispatches is written against a series that does not
// exist until the first one occurs, which is exactly when the alert is
// needed. Prime exists for this; a new label value that skips it is the
// easy mistake.
func TestEveryDispatchOutcomeIsPublishedBeforeItHappens(t *testing.T) {
	obs := observability.New()
	obs.Prime("triage")
	for _, outcome := range []string{
		observability.DispatchOK,
		observability.DispatchHalted,
		observability.DispatchRateLimited,
		observability.DispatchFailed,
	} {
		if got := scrapeCounter(t, obs, `mast_dispatches_total{outcome="`+outcome+`",workload="triage"}`); got != "0" {
			t.Errorf("mast_dispatches_total{outcome=%q} = %s before any dispatch, want 0", outcome, got)
		}
	}
}
