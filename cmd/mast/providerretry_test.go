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
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"iter"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/mast/internal/modelretry"
	"github.com/go-steer/mast/pkg/observability"
)

// retryReporting builds the reporter wired to a real registry and a
// logger the test can read.
func retryReporting(t *testing.T) (func(modelretry.Event), *observability.Registry, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	obs := observability.New()
	// As main.go does at startup, so the "and not the other label"
	// assertions read a published zero rather than an absence.
	obs.Prime("triage")
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return providerRetryReporter(obs, "triage", logger), obs, &buf
}

// TestARecoveredCallIsCountedAndNamed. The whole hazard this family
// answers is that a recovered call is invisible: the turn completes,
// returns content, and looks unremarkable except for being two seconds
// slower. Without a report, a provider shedding load harder every week
// produces green, increasingly slow turns with nothing to point at.
func TestARecoveredCallIsCountedAndNamed(t *testing.T) {
	report, obs, logs := retryReporting(t)

	report(modelretry.Event{
		Model: "gemini-3.7-flash", Outcome: modelretry.Recovered,
		Attempts: 1, Waited: 2 * time.Second,
		Err: errors.New("googleapi: Error 429: Resource has been exhausted (e.g. check quota)., RESOURCE_EXHAUSTED"),
	})

	if got := scrapeCounter(t, obs, `mast_provider_retries_total{outcome="recovered",workload="triage"}`); got != "1" {
		t.Errorf("recovered retries = %s, want 1", got)
	}
	line := logs.String()
	for _, want := range []string{"gemini-3.7-flash", "outcome=recovered", "retries=1", "waited=2s", "RESOURCE_EXHAUSTED"} {
		if !strings.Contains(line, want) {
			t.Errorf("the log line does not carry %q:\n%s", want, line)
		}
	}
}

// TestEachRetryOutcomeGetsItsOwnLabel. The three are different
// operational facts and an alert that cannot tell them apart is one
// nobody can act on: recovered is the provider shedding and mast
// absorbing it, exhausted is work that ended short, and declined is
// mast's own cooldown refusing — a fan-out wider than its quota, which
// is a workload-shape problem rather than a provider problem.
func TestEachRetryOutcomeGetsItsOwnLabel(t *testing.T) {
	for _, tc := range []struct {
		outcome modelretry.Outcome
		want    string
	}{
		{modelretry.Recovered, "recovered"},
		{modelretry.Exhausted, "exhausted"},
		{modelretry.Declined, "declined"},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			report, obs, _ := retryReporting(t)
			report(modelretry.Event{Model: "m", Outcome: tc.outcome, Err: errors.New("429")})

			for _, label := range []string{"recovered", "exhausted", "declined"} {
				want := "0"
				if label == tc.want {
					want = "1"
				}
				metric := fmt.Sprintf(`mast_provider_retries_total{outcome=%q,workload="triage"}`, label)
				if got := scrapeCounter(t, obs, metric); got != want {
					t.Errorf("%s = %s, want %s", metric, got, want)
				}
			}
		})
	}
}

// TestEveryRetryOutcomeIsPublishedBeforeItHappens. An alert on a series
// that does not exist yet does not fire, and a dashboard panel for one
// reads "no data" rather than zero — so a workload that has never met a
// rejection has to say so, not say nothing. Prime is what does it;
// this is the assertion that the new family was added to its list.
func TestEveryRetryOutcomeIsPublishedBeforeItHappens(t *testing.T) {
	_, obs, _ := retryReporting(t)
	for _, label := range []string{"recovered", "exhausted", "declined"} {
		metric := fmt.Sprintf(`mast_provider_retries_total{outcome=%q,workload="triage"}`, label)
		if got := scrapeCounter(t, obs, metric); got != "0" {
			t.Errorf("%s = %s before anything happened, want a published 0", metric, got)
		}
	}
}

// shedsOnce rejects its first call with the error the 2026-08-21
// nightly received, then answers.
type shedsOnce struct{ calls int }

func (m *shedsOnce) Name() string { return "gemini-3.7-flash" }

func (m *shedsOnce) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.calls++
	first := m.calls == 1
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if first {
			yield(nil, fmt.Errorf("failed to call model: %w", genai.APIError{
				Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "Resource exhausted. Please try again later.",
			}))
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel)}, nil)
	}
}

// TestTheDaemonsStartupActuallyWiresTheProcessPolicy is the half that
// keeps the rest of this file from being a test of a function nobody
// calls.
//
// It installs through the real entry point and then drives a rejection
// through modelretry.Shared() — the same policy
// compose.NewRuntimeModel wraps every runtime model in, which
// internal/compose.TestARuntimeModelIsDrivenByTheProcessPolicy pins.
// The two together are the claim: a 429 on any model this daemon builds
// reaches this counter.
//
// It costs the policy's real two-second wait, and that is the price of
// running the production schedule rather than a test one. Faking the
// clock here would test a policy the daemon does not install.
func TestTheDaemonsStartupActuallyWiresTheProcessPolicy(t *testing.T) {
	var buf bytes.Buffer
	obs := observability.New()
	obs.Prime("triage")
	reportProviderRetries(obs, "triage",
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	inner := &shedsOnce{}
	for _, err := range modelretry.Shared().Wrap(inner).GenerateContent(
		context.Background(), &adkmodel.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("the call did not recover: %v", err)
		}
	}
	if inner.calls != 2 {
		t.Fatalf("the model was called %d time(s), want 2 — the shared policy did not retry", inner.calls)
	}
	if got := scrapeCounter(t, obs, `mast_provider_retries_total{outcome="recovered",workload="triage"}`); got != "1" {
		t.Errorf("recovered retries = %s, want 1: the daemon's observer is not on the policy its models use", got)
	}
}

// TestStartupInstallsTheReporterWhereItPrimesTheRegistry closes the last
// gap in the chain the test above walks: that test proves the reporter
// works when installed, and this one proves the daemon installs it.
// Without this, deleting the one line in main.go leaves every test in
// this file green while the shipped binary reports nothing.
//
// It asserts co-location with obs.Prime rather than mere presence,
// because those two calls have to happen in the same place for the same
// reason — the reporter needs the workload name and the registry, and
// priming is what proves both exist by then.
func TestStartupInstallsTheReporterWhereItPrimesTheRegistry(t *testing.T) {
	src, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}
	for _, decl := range src.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var primes, reports bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				primes = primes || fun.Sel.Name == "Prime"
			case *ast.Ident:
				reports = reports || fun.Name == "reportProviderRetries"
			}
			return true
		})
		if primes && reports {
			return
		}
		if primes != reports {
			t.Errorf("%s primes=%v installs-the-retry-reporter=%v: these belong together",
				fn.Name.Name, primes, reports)
		}
	}
	t.Fatal("no function in main.go both primes the registry and installs the provider-retry reporter")
}
