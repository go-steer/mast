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
	"strings"

	"google.golang.org/adk/v2/model"

	"github.com/go-steer/mast/pkg/providers/anthropic"
	geminiprov "github.com/go-steer/mast/pkg/providers/gemini"
	"github.com/go-steer/mast/pkg/workload"
)

// The provider's server-side built-in tools, and mast's posture on
// them (#324, resolved 2026-09-10).
//
// They are invisible to every control mast has. A built-in runs inside
// the vendor's infrastructure and its result arrives folded into the
// response, so it never becomes a FunctionCall: the permissions gate
// has nothing to allow, the write gate has nothing to park, the effect
// outbox has nothing to record, and the transcript shows a model that
// simply knew something. A specialist declared read_only — a
// declaration CheckCapabilitySplit verifies and the write gate
// measures — could still read the public internet.
//
// Two decisions live here.
//
// First, mast's baseline is OFF for every provider, rather than each
// vendor's own baseline. Gemini ships google_search and url_context on;
// Anthropic ships web_search off. Inheriting those would mean one mast
// image changing an unattended agent's reach with a --provider flag,
// and "the same config runs Gemini or Claude" is a pillar. It also
// means the paths with no bundle to read — `mast run`, a library embed
// that sets only ModelName — are safe by construction rather than by
// remembering to write a key.
//
// Second, the gate is bundle-scoped and there is deliberately no
// per-specialist axis. Upstream core-agent merges the block per field
// from parent to subagent, guarding against a subagent that names one
// key silently reclaiming the others. That trap cannot occur here: the
// baseline is off and only the bundle can turn a tool on, so there is
// nothing for a specialist to lose or hand back. Adding the axis would
// also mean changing specialists.ModelResolver — which is keyed by
// model name and does not know which specialist is asking — on a path
// #300 freezes at v1.0. If a roster ever needs one analyst grounded and
// the rest not, that is the change to make, and it fails safe until
// then.

// geminiBuiltins maps the neutral block onto Gemini's toggles. All
// three names have an equivalent here.
func geminiBuiltins(bt workload.BuiltinTools) geminiprov.BuiltinTools {
	return geminiprov.BuiltinTools{
		GoogleSearch:  bt.WebSearchOn(),
		URLContext:    bt.URLContextOn(),
		CodeExecution: bt.CodeExecutionOn(),
	}
}

// anthropicBuiltins maps the neutral block onto Anthropic's toggles.
// Only web_search has an equivalent; url_context and code_execution
// land nowhere, which is fail-safe by construction — a tool this
// provider cannot send is a tool it cannot leave on. BuiltinToolsSummary
// is how that gap becomes visible instead of assumed.
func anthropicBuiltins(bt workload.BuiltinTools) anthropic.BuiltinTools {
	return anthropic.BuiltinTools{WebSearch: bt.WebSearchOn()}
}

// BuiltinToolsReporter is the optional extension a backend implements
// when it injects the provider's own server-side tools into every
// request. It reports the effective set under the provider-neutral
// names the bundle uses.
//
// Owned here, at the consumer, rather than in a provider package:
// pkg/providers/gemini and pkg/providers/anthropic satisfy it
// structurally and import nothing to do so, and the two backends stay
// unable to see each other. Backends with no such concept (echo,
// scripted, toolactor) simply do not implement it.
type BuiltinToolsReporter interface {
	BuiltinToolNames() []string
}

// BuiltinToolsSummary renders a constructed model's effective
// server-side built-in set for an operator-facing log line: the
// comma-joined neutral names, or "none" when the backend has the
// concept and everything is off.
//
// Returns "" — not "none" — for a backend with no server-side built-in
// concept at all, so a caller can omit the field rather than assert an
// absence that means nothing there.
//
// Read off the constructed model rather than off the bundle, and that
// is the point rather than a detail. YAML decoding does not reject
// unknown keys, so `builtin_tols: {web_search: true}` is discarded in
// silence; a line derived from the same struct that was discarded would
// agree with the typo. This one answers the question an operator
// actually has — what will the model send — from the far side of the
// constructor.
func BuiltinToolsSummary(m model.LLM) string {
	r, ok := m.(BuiltinToolsReporter)
	if !ok {
		return ""
	}
	names := r.BuiltinToolNames()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ",")
}
