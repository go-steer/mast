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

// Package cli is the mast binary, for building your own.
//
// The stock binary is one line over this package:
//
//	func main() { os.Exit(cli.Main(context.Background(), os.Args[1:])) }
//
// A custom main.go is the same line with options. The result is the same
// binary — the same flags, subcommands, servers, governance and exit
// codes — with your models and tools added:
//
//	func main() {
//		os.Exit(cli.Main(context.Background(), os.Args[1:],
//			cli.WithModels(func(name string) (model.LLM, bool) {
//				if strings.HasPrefix(name, "acme-") {
//					return acme.New(name), true
//				}
//				return nil, false // fall through to mast's providers
//			}),
//			cli.WithTools("tickets", openTicket, closeTicket),
//		))
//	}
//
// Every option takes ADK's own types (model.LLM, tool.Tool, tool.Toolset),
// so nothing here commits you to a mast type beyond the options
// themselves.
//
// What you add is governed like what mast ships.
//
// A model you supply is metered from the usage it reports on each
// response, like any other, and held to the workload's budget by it; a
// model that reports no usage cannot be held to a cost ceiling by
// anything. If mast's pricing catalog does not know the model, its calls
// are priced at the fallback rate and counted as unpriced rather than as
// free.
//
// A tool you supply is a toolset, reached the way an MCP server is: by a
// specialist whose tools.mcp allowlist names it, or by one that declares
// no tools.mcp at all and so inherits every toolset — which startup
// refuses for a read-only specialist. mast classifies a tool it does not
// know as mutating, so the workload's on_mutation policy applies to its
// calls (by default they park for an operator's approval) until the
// workload's tool_catalog.tools marks it read-only.
package cli

import (
	"context"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	internalcli "github.com/go-steer/mast/internal/cli"
)

// Main runs the mast binary over args — the command line without the
// program name, os.Args[1:] — and returns the process exit status for
// the caller to hand to os.Exit. The flags, subcommands and exit codes
// are the ones the stock binary documents.
//
// Cancelling ctx shuts serve mode down the way SIGINT or SIGTERM does:
// the same drain, the same exit status. Main does not call os.Exit
// itself, with one exception: if serve mode's teardown hangs past its
// deadline after the drain, a watchdog dumps every goroutine's stack
// and exits the process, because nothing is left to return to.
func Main(ctx context.Context, args []string, opts ...Option) int {
	var ext internalcli.Options
	for _, o := range opts {
		if o.apply != nil {
			o.apply(&ext)
		}
	}
	return internalcli.MainWith(ctx, args, ext)
}

// Option is something a custom main.go adds to the binary. Build one
// with a With function; the zero Option adds nothing.
type Option struct {
	apply func(*internalcli.Options)
}

// WithModels adds a model resolver, asked for every model name before
// mast's own providers: the --model value, and each specialist's
// `model:` override. Return the model and true to supply it, or false to
// fall through to mast's providers.
//
// Given more than once, the resolvers are asked in the order given and
// the first that answers wins.
//
// A model you return is used as you built it: mast does not wrap it in
// its own retry, because your client owns that. Under --model=echo or
// another offline fake the whole process is a test double and every
// specialist's model collapses to the fake, yours included.
func WithModels(lookup func(name string) (model.LLM, bool)) Option {
	return Option{apply: func(o *internalcli.Options) {
		if lookup == nil {
			return
		}
		prev := o.Models
		o.Models = func(name string) (model.LLM, bool) {
			if prev != nil {
				if m, ok := prev(name); ok {
					return m, true
				}
			}
			return lookup(name)
		}
	}}
}

// WithToolset adds a toolset, offered to every Task-mode specialist whose
// tools.mcp allowlist names it — `- server: <Name()>` — exactly as it
// would name an MCP server, including the optional tools: list that
// narrows it to particular tools. Startup refuses an allowlist naming a
// toolset that does not exist, a toolset whose name is empty or taken by
// another toolset or by an MCP server in the workload's catalog, and a
// read-only specialist that would reach a tool it has not enumerated.
func WithToolset(ts tool.Toolset) Option {
	return Option{apply: func(o *internalcli.Options) {
		if ts != nil {
			o.Toolsets = append(o.Toolsets, ts)
		}
	}}
}

// WithTools adds tools as one toolset called name — WithToolset for when
// you have tools rather than a toolset. A specialist reaches them by
// naming it in tools.mcp.
func WithTools(name string, tools ...tool.Tool) Option {
	return WithToolset(staticToolset{name: name, tools: append([]tool.Tool(nil), tools...)})
}

// staticToolset is a fixed list of tools under one name.
type staticToolset struct {
	name  string
	tools []tool.Tool
}

func (s staticToolset) Name() string { return s.name }

func (s staticToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) {
	return s.tools, nil
}
