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

// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818

package anthropic

import (
	"github.com/anthropics/anthropic-sdk-go"
)

// BuiltinTools toggles Anthropic's server-side built-in tools surfaced
// by this provider. Each enabled flag becomes one entry on the
// request's Tools slice alongside any user-defined function
// declarations.
//
// Defaults: everything OFF. Anthropic's server-side tools are billed
// per use on top of token cost (web_search is per-search), so we apply
// the same "active surface = opt in" rule that keeps Gemini's
// CodeExecution off by default. The library caller decides explicitly
// whether the cost and external-action posture are acceptable.
//
// To turn one on:
//
//	provider, _ := anthropic.New(anthropic.Options{
//	    APIKey:       key,
//	    BuiltinTools: anthropic.BuiltinTools{WebSearch: true},
//	})
//
// Other Anthropic server-side tools (web_fetch, code_execution,
// text_editor, bash, memory) aren't surfaced today. Add them under
// the same struct when a concrete consumer needs one.
type BuiltinTools struct {
	WebSearch bool // Server-side web search; per-search billing on top of tokens.
}

// DefaultBuiltinTools returns the on-by-default baseline applied to
// every Provider unless overridden via Options.BuiltinTools /
// VertexOptions.BuiltinTools. Currently empty (and identical to the
// zero value) — see the BuiltinTools doc for why.
func DefaultBuiltinTools() BuiltinTools {
	return BuiltinTools{}
}

// asAnthropicTools projects the toggles into the SDK's ToolUnionParam
// shape. Order matches the field order in the struct so the request
// shape is deterministic across runs (matters for prompt caching).
func (b BuiltinTools) asAnthropicTools() []anthropic.ToolUnionParam {
	var out []anthropic.ToolUnionParam
	if b.WebSearch {
		out = append(out, anthropic.ToolUnionParam{
			OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{},
		})
	}
	return out
}

// Names reports the enabled built-ins under the provider-neutral names
// mast's `builtin_tools:` block uses (see pkg/workload.BuiltinTools),
// so a report reads the same whichever provider is resolved and an
// operator can match it against the keys they typed.
//
// The block's url_context and code_execution are Gemini-side and have
// no Anthropic equivalent, so they can never appear here. That is the
// direction that fails safe: a tool this provider cannot send is one
// it cannot leave on.
func (b BuiltinTools) Names() []string {
	var out []string
	if b.WebSearch {
		out = append(out, "web_search")
	}
	return out
}

// BuiltinToolNames reports the server-side built-ins this model will
// send, under the neutral names. Recognized by duck-typing in the
// consuming package (internal/compose's BuiltinToolsReporter) — the
// method name is a cross-package contract; do not rename.
//
// On the model rather than the Provider because that is what mast's
// composer hands back: BuildModel returns Provider.Model(...), and a
// report read off anything the caller does not hold could drift from
// the requests it is meant to describe.
func (l *llm) BuiltinToolNames() []string { return l.builtins.Names() }
