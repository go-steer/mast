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

package cli

import (
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
)

// Options is what a custom main.go adds to the binary, through the public
// github.com/go-steer/mast/cli package. The zero value is the stock
// `mast` binary: cmd/mast passes nothing.
//
// Every field is in ADK's own types. The public package keeps its option
// type opaque and converts to this one, so nothing exported names it.
type Options struct {
	// Models is asked for every model name before mast's own providers:
	// --model, and each specialist's `model:` override. Returning false
	// falls through.
	Models func(name string) (model.LLM, bool)

	// Toolsets are offered to Task-mode specialists beside the
	// workload's MCP servers, reached by naming a toolset's Name() in a
	// specialist's tools.mcp allowlist.
	Toolsets []tool.Toolset
}

// lookupModel asks the binary's resolver for name, reporting whether it
// answered.
func (o Options) lookupModel(name string) (model.LLM, bool) {
	if o.Models == nil {
		return nil, false
	}
	m, ok := o.Models(name)
	return m, ok && m != nil
}
