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

package compose

import (
	"context"
	"errors"

	"github.com/go-steer/core-models/kvmetrics"
	"github.com/go-steer/core-models/profile"
)

// KVSampler returns a KV-cache sampler for the self-hosted server a
// provider profile names, or nil when there is nothing to sample: the
// provider is not a profile, or the profile declares no metrics_url, or
// its server exposes no KV statistics (Ollama, metrics: none). A nil
// sampler with a nil error is the common case and means "skip".
//
// The window it reports is fleet-level and time-correlated — the
// server's prefix cache across all its traffic — so callers report it
// beside usage and never fold it into cost (core-models design §8). The
// model id narrows the reading to that model's series when the server
// labels them, as vLLM and SGLang do.
func KVSampler(ctx context.Context, provider, modelName string) (*kvmetrics.Sampler, error) {
	p, ok, err := LookupProfile(provider)
	if err != nil || !ok {
		return nil, err
	}
	if p.MetricsURL == "" || p.Metrics == "" || p.Metrics == profile.MetricsNone {
		return nil, nil
	}
	r, err := profile.Resolve(ctx, p, profile.Options{})
	if err != nil {
		return nil, err
	}
	s, err := kvmetrics.FromProfile(r, kvmetrics.Options{Model: modelName})
	if errors.Is(err, kvmetrics.ErrNoMetrics) {
		return nil, nil
	}
	return s, err
}
