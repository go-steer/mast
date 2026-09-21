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
	"log/slog"

	"github.com/go-steer/mast/internal/modelretry"
	"github.com/go-steer/mast/pkg/observability"
)

// reportProviderRetries points the process-wide retry policy at this
// daemon's registry and logger (#452).
//
// # Why this is installed and not constructed
//
// The wrapping happens in compose.NewRuntimeModel, which runs wherever
// a model is built — including inside the specialist resolver, on first
// dispatch to a `model:` override, long after startup. It cannot be
// handed a registry or a workload name, so the policy is built with
// neither and told where to report once, here, by the one component
// that knows both. Same late-binding shape as daemonSubRunObserver.attach.
//
// A library embed reaches neither this call nor a registry of its own,
// so it gets the retry and no report. That is the pre-existing shape of
// mast's library surface rather than a decision taken here:
// pkg/observability is a daemon concern, and an embedding host that
// wants these numbers owns its own metrics.
func reportProviderRetries(obs *observability.Registry, workload string, logger *slog.Logger) {
	modelretry.Shared().SetObserver(providerRetryReporter(obs, workload, logger))
}

// providerRetryReporter is what gets installed: one report per model
// call that met a transient provider rejection.
//
// Two reports per event, because neither is sufficient alone. The
// counter is what an alert fires on and cannot name a model; the log
// line names the model and the wait and cannot be aggregated.
//
// Logged at every outcome, including the one that worked. A recovered
// call is the only trace that the provider is shedding at all — the
// turn it belongs to completes, returns content, and looks unremarkable
// except for being two seconds slower, which is precisely how a
// provider under worsening pressure stays invisible until it stops
// being transient.
func providerRetryReporter(obs *observability.Registry, workload string, logger *slog.Logger) func(modelretry.Event) {
	return func(ev modelretry.Event) {
		obs.ProviderRetry(workload, providerRetryOutcome(ev.Outcome))
		if logger == nil {
			return
		}
		// ev.Err is always non-nil — the policy emits nothing for a call
		// that met no rejection — but this runs on every model call in
		// the process, so it does not dereference on that promise.
		reason := "(none)"
		if ev.Err != nil {
			reason = ev.Err.Error()
		}
		logger.Warn("a model call met a transient provider rejection",
			"model", ev.Model, "outcome", string(ev.Outcome),
			"retries", ev.Attempts, "waited", ev.Waited.String(),
			"error", reason)
	}
}

// providerRetryOutcome maps a policy outcome to its metric label.
//
// Exhaustive by construction rather than by switch coverage: an outcome
// this does not know about counts as exhausted, which is the
// conservative reading — "the call met a rejection and did not recover"
// — rather than a dropped count or a new label minted at runtime.
func providerRetryOutcome(out modelretry.Outcome) string {
	switch out {
	case modelretry.Recovered:
		return observability.ProviderRetryRecovered
	case modelretry.Declined:
		return observability.ProviderRetryDeclined
	default:
		return observability.ProviderRetryExhausted
	}
}
