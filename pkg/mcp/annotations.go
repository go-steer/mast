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

// Originally derived from go-steer/core-agent@920918836ddff082289bc0f65bd415923c11f096:pkg/mcp/readonlyhint.go

package mcp

import (
	"context"
	"log/slog"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// methodListTools is the JSON-RPC method whose result carries tool
// annotations. The SDK's own constant is unexported, so this is a copy;
// it is protocol vocabulary, not an SDK detail, so it cannot drift
// without the wire changing.
const methodListTools = "tools/list"

// Annotations records the `readOnlyHint` an MCP server publishes for
// each of its tools, so mast's mutation predicate can classify an MCP
// tool instead of defaulting it to mutating.
//
// # Why this exists at all
//
// ADK's mcptoolset drops MCP annotations: convertTool copies name,
// description and schemas and nothing else. That is true, and mast's
// own comments used to draw the wrong conclusion from it — that
// default-deny-unknown was the only implementable stance, so every MCP
// tool parks under the default hitl.on_mutation: require_approval. The
// annotation was never missing from the wire. It is dropped *after* the
// adapter already received it, in a conversion mast does not have to
// go through: the tools/list response passes through this client's own
// sending middleware on its way back, with the annotations still on it.
//
// One session, one round trip. Opening a second client to ask the same
// question again would fork a second process for a stdio server.
//
// # Absence is carried in the shape
//
// mcpsdk.ToolAnnotations.ReadOnlyHint is a plain bool, so "the server
// said false" and "the server said nothing" are the same value. A tool
// whose Annotations object is nil is therefore not recorded at all,
// rather than recorded as mutating: the predicate's own default already
// covers it, and an entry would make an unclassified tool look
// classified to anyone reading this registry.
//
// Within a non-nil Annotations object the distinction does not arise —
// MCP specifies readOnlyHint's default as false, which is mast's
// default too, so an annotations object that omits the field decodes to
// the same answer both readings give.
//
// # Trust
//
// A readOnlyHint is the server's word for it. mast takes that word
// because mcp.json is operator-trusted control-plane config — the
// permission gate write-protects it, and adding a server to it is
// already the grant. Where an operator does not want to extend that
// trust to a particular tool, the audited workload override
// (tool_catalog.tools[].mutating) is checked BEFORE this registry and
// still wins. See the resolved-decisions table in docs/README.md.
//
// Safe for concurrent use: the pump of tools/list responses and the
// predicate's readers run on different goroutines.
type Annotations struct {
	log *slog.Logger

	mu    sync.RWMutex
	hints map[string]annotation
	seen  map[string]bool // servers whose first tools/list has been logged
}

// annotation is one tool's recorded classification plus who said so, so
// a conflict between two servers can name both sides.
type annotation struct {
	readOnly bool
	server   string
}

// NewAnnotations returns an empty registry. A nil logger is replaced
// with slog.Default(); a nil *Annotations is usable for every read
// (ReadOnly reports nothing known), so a caller that does not want
// annotation-based classification passes nil rather than branching.
func NewAnnotations(log *slog.Logger) *Annotations {
	if log == nil {
		log = slog.Default()
	}
	return &Annotations{log: log, hints: map[string]annotation{}, seen: map[string]bool{}}
}

// ReadOnly reports the readOnlyHint recorded for toolName. annotated is
// false when no server has published annotations for that name — which
// includes the case where no tools/list has happened yet, because MCP
// sessions are established lazily on first use.
//
// That lazy window is not a gap in practice: the model can only call a
// tool it was shown, and it is shown tools by the same tools/list whose
// response populates this registry. A classification consulted before
// any list — composition-time validation, for one — sees nothing known
// and gets mast's default-deny-unknown answer, which is what it got
// before this registry existed.
func (a *Annotations) ReadOnly(toolName string) (readOnly, annotated bool) {
	if a == nil {
		return false, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	h, ok := a.hints[toolName]
	return h.readOnly, ok
}

// observe records the annotations on one tools/list response.
//
// Tool names are un-namespaced in mast (#221), so two servers can
// publish the same name. A disagreement resolves toward mutating and is
// logged at WARN naming both servers: the gate's job is to be wrong in
// the direction that asks a human, and an operator who meant two
// different tools to share a name has a bug either way.
func (a *Annotations) observe(server string, tools []*mcpsdk.Tool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	var readOnly, mutating, unannotated int
	for _, t := range tools {
		if t == nil || t.Annotations == nil {
			unannotated++
			continue
		}
		if t.Annotations.ReadOnlyHint {
			readOnly++
		} else {
			mutating++
		}
		prev, seen := a.hints[t.Name]
		if seen && prev.server != server && prev.readOnly != t.Annotations.ReadOnlyHint {
			a.log.Warn("MCP servers disagree on a tool's readOnlyHint; classifying it mutating",
				"tool", t.Name, "servers", []string{prev.server, server})
			a.hints[t.Name] = annotation{readOnly: false, server: server}
			continue
		}
		a.hints[t.Name] = annotation{readOnly: t.Annotations.ReadOnlyHint, server: server}
	}

	// The operator is told what the server claimed, once per server.
	// Classification moving off default-deny-unknown on somebody else's
	// say-so is worth a line in the log; repeating it on every
	// connection refresh is not.
	if a.seen[server] {
		a.log.Debug("MCP tool annotations refreshed",
			"server", server, "read_only", readOnly, "mutating", mutating, "unannotated", unannotated)
		return
	}
	a.seen[server] = true
	a.log.Info("MCP tool annotations captured",
		"server", server, "read_only", readOnly, "mutating", mutating, "unannotated", unannotated)
}

// captureToolAnnotations reads the annotations off every tools/list
// response on its way back to the caller, and changes nothing about it.
//
// It sits on the sending side, next to refuseInputRequiredResults, for
// the same reason that one does: this is the edge where mast sees what
// the server actually sent, before ADK's conversion throws the
// annotations away.
func captureToolAnnotations(a *Annotations, server string) mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != methodListTools {
				return res, err
			}
			if list, ok := res.(*mcpsdk.ListToolsResult); ok {
				a.observe(server, list.Tools)
			}
			return res, nil
		}
	}
}
