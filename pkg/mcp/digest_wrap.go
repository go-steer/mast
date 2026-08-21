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

// Derived from go-steer/core-agent's pkg/mcp/digest_wrap.go, ported to
// ADK v2 and to mast's un-namespaced toolset shape (#221).

package mcp

import (
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/digest"
)

// tracer is the OTel tracer for the MCP wrap layer. The mcp.tool_call
// span it opens is the parent of the digest.process span pkg/digest
// emits. Resolved once — a no-op when the global provider is noop,
// which is the default.
//
// It does not yet parent the upstream HTTP round trip: mast has no
// otelhttp on the MCP transport (see newHTTPToolset's note that the
// span layer belongs outside jsonRPCErrorTransport when it arrives).
// Upstream swaps a span-carrying context into the tool.Context to get
// that nesting; with nothing to nest there is nothing to swap, and the
// shim is deliberately not ported. It comes back with otelhttp.
var tracer = otel.Tracer("mast/mcp")

// DefaultDigestThreshold is the response size, in bytes, below which a
// tool response bypasses the router entirely and is returned verbatim.
// Small responses are the common case and digesting them buys nothing
// but latency and a wrapped shape the model has to read through.
const DefaultDigestThreshold = 8000

// DigestOptions configures how WithDigest routes MCP tool responses
// through pkg/digest. A nil *DigestOptions disables wrapping entirely,
// which is what --mcp-digest=false hands the wiring site.
//
// There is no LLMFallback field. Upstream has one — an opt-in
// small-tier subagent that compresses prose the structural pruner
// cannot reduce — and mast does not port it: it would need a resolved
// small-tier model, a second billing path inside a tool call, and a
// budget story for spend the session's meter never sees. mast's wrap
// is structural-only, so a payload the pruner cannot reduce takes
// pkg/digest's bounded passthrough (digest.MaxPassthroughBytes) and
// digest.Savings.Subagent* stay zero here by construction.
type DigestOptions struct {
	// Store is the CCR backing for retrieve_raw. When nil, digesting
	// still runs but the raw payload is gone once the digest is made —
	// so the wiring site pairs a store with the retrieve_raw tool and
	// registers neither without the other.
	Store digest.Store

	// Threshold is the serialized byte size below which a response is
	// returned unwrapped — not digested-then-passed-through, but
	// handed back in the tool's own shape. Zero means
	// DefaultDigestThreshold. See Run for why the distinction matters.
	Threshold int

	// NeverServers names catalog servers (by mcp.json key) that opt out
	// of digesting, from their `no_digest: true`. The check runs at
	// wrap time rather than per call, so an opted-out server's tools
	// are the unwrapped originals — no per-call branch, and nothing
	// downstream can tell a never-server's tool from an unwrapped one.
	NeverServers map[string]bool

	// OnResult, when non-nil, fires after every successful Process call
	// with the fully-populated Result. Runs synchronously on the tool's
	// own goroutine, so a callback that does I/O adds latency to every
	// tool call: increment counters, nothing more.
	OnResult func(*digest.Result)
}

// threshold returns the effective threshold, defaulting a zero value.
func (o *DigestOptions) threshold() int {
	if o == nil || o.Threshold <= 0 {
		return DefaultDigestThreshold
	}
	return o.Threshold
}

// WithDigest wraps ts so every tool response it serves is routed
// through digest.Process before the model sees it. server is the
// mcp.json key, checked against opts.NeverServers.
//
// It returns ts unchanged when there is nothing to do: a nil toolset,
// a nil opts (digesting off), or a server that opted out. That makes
// the wrap safe to apply unconditionally at the call site, which is
// where the decision is easiest to read.
func WithDigest(ts tool.Toolset, server string, opts *DigestOptions) tool.Toolset {
	if ts == nil || opts == nil || opts.NeverServers[server] {
		return ts
	}
	return &digestingToolset{inner: ts, opts: opts}
}

// digestingToolset wraps every runnable tool its inner toolset serves.
//
// Name() delegates, which matters more here than it looks: pkg/mcp's
// `named` wrapper exists so a per-specialist `tools.mcp: - server:`
// allowlist can match a toolset to its catalog key, and
// specialists.filterToolsets does that match on Name(). A wrap that
// renamed — or defaulted — the toolset would silently empty every
// enumerated allowlist, which is exactly the bug `named` was added to
// fix.
type digestingToolset struct {
	inner tool.Toolset
	opts  *DigestOptions
}

func (d *digestingToolset) Name() string { return d.inner.Name() }

// Tools wraps each runnable tool. A tool that is not runnable — one
// with no Declaration/Run pair — is passed through untouched, the same
// concession ADK's own tool.WithConfirmation makes: there is no
// response to digest on something that never runs.
func (d *digestingToolset) Tools(ctx adkagent.ReadonlyContext) ([]tool.Tool, error) {
	upstream, err := d.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tool.Tool, 0, len(upstream))
	for _, t := range upstream {
		if rt, ok := t.(runnableTool); ok {
			out = append(out, &digestingTool{runnableTool: rt, opts: d.opts})
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// runnableTool is the tool shape ADK's LLM flow actually dispatches:
// tool.Tool plus a declaration the model sees and a Run the flow
// calls. ADK declares the same interface unexported in tool.go; it is
// restated here because a wrapper cannot be written without it.
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx adkagent.Context, args any) (map[string]any, error)
}

// digestingTool routes one tool's response through digest.Process.
// Name, Description, IsLongRunning and Declaration all come from the
// embedded original, so the model sees the same tool it would have
// seen; only what comes back changes.
type digestingTool struct {
	runnableTool
	opts *DigestOptions
}

// ProcessRequest keeps the wrapper on the dispatch path.
//
// ADK's flow packs tools into req.Tools by name and later looks the
// call up in that map, so a tool that packs *itself* would put the
// undigested original back and the wrap would be inert — the model
// would see a declaration from the wrapper and a response from the
// inner tool. mcptoolset's tool does implement ProcessRequest, so this
// runs the inner packer for its side effects and then overwrites the
// entry with the wrapper. Same shape, and same reason, as ADK's
// confirmationTool.
func (d *digestingTool) ProcessRequest(ctx adkagent.Context, req *model.LLMRequest) error {
	type requestProcessor interface {
		ProcessRequest(ctx adkagent.Context, req *model.LLMRequest) error
	}
	if rp, ok := d.runnableTool.(requestProcessor); ok {
		_, existedBefore := req.Tools[d.Name()]
		if err := rp.ProcessRequest(ctx, req); err != nil {
			return err
		}
		if !existedBefore && req.Tools != nil && req.Tools[d.Name()] != nil {
			req.Tools[d.Name()] = d
			return nil
		}
	}
	return toolutils.PackTool(req, d)
}

// Run calls the wrapped tool and, when the response is large enough to
// be worth it, returns a digest of it instead:
//
//	{
//	  "digest":     "<compressed payload>",
//	  "raw_bytes":  N,
//	  "method":     "structural_json" | "passthrough",
//	  "latency_ms": N,
//	  "call_id":    "<function call id>",   // only with a Store
//	  "savings":    {...},                  // byte + token reduction
//	  "digest_meta": {...},                 // pruner stats, when any
//	}
//
// The model reads `digest` as the tool's answer and can hand `call_id`
// to retrieve_raw when the digest looks like it dropped something it
// needs. That escape hatch is the reason a Store and the retrieve_raw
// tool are registered together or not at all.
//
// Every failure degrades to the undigested response rather than to an
// error: a tool call that worked must not fail because the thing that
// was supposed to make it *smaller* did not. The upstream tool's own
// error and its own response come back verbatim on those paths.
//
// Verbatim is load-bearing, not tidiness. The wrap adds nothing — not
// even a latency sidecar — to a response it does not digest, because
// mast compares two reads of the same tool for equality: a change-set
// grant is voided when its precondition read stops returning what it
// returned at approval time (pkg/approval). A wall-clock field in an
// otherwise unchanged response makes a still cluster look like a moved
// one, roughly whenever the two calls land in different milliseconds.
// Latency is on the span, where it costs no caller anything.
func (d *digestingTool) Run(ctx adkagent.Context, args any) (map[string]any, error) {
	spanCtx, span := tracer.Start(ctx, "mcp.tool_call", trace.WithSpanKind(trace.SpanKindInternal))
	span.SetAttributes(attribute.String("mast.mcp.tool_name", d.Name()))
	defer span.End()

	start := time.Now()
	raw, err := d.runnableTool.Run(ctx, args)
	latencyMS := time.Since(start).Milliseconds()
	if err != nil {
		return raw, err
	}

	rawBytes, marshalErr := json.Marshal(raw)
	if marshalErr != nil {
		return raw, nil
	}

	// Under the threshold, hand the tool's own response straight back.
	// This is a deliberate divergence from upstream, which wraps every
	// response and lets pkg/digest's router choose passthrough.
	//
	// The router's answer would be the same. What differs is the shape
	// the model reads: a wrapped passthrough re-serializes the
	// response's map into a JSON *string* under a "digest" key, so a
	// three-field answer arrives as escaped text the model has to
	// unpick, and costs more tokens than the map it replaced. Doing
	// that to the small responses — the overwhelming majority of tool
	// calls — to compress the large ones inverts the point. Above the
	// threshold the wrap is worth its shape; below it, nothing was
	// dropped, so there is nothing to explain to the model and no raw
	// payload worth storing.
	//
	// One consequence, and it is the honest reading: digest.Telemetry()
	// (the /usage digest_methods block) counts routed calls, not tool
	// calls. A daemon whose responses are all small reports no digest
	// block at all.
	if len(rawBytes) < d.opts.threshold() {
		return raw, nil
	}

	res, procErr := digest.Process(spanCtx, rawBytes, digest.Options{
		Threshold: d.opts.threshold(),
		Store:     d.opts.Store,
		CallID:    ctx.FunctionCallID(),
	})
	if procErr != nil {
		return raw, nil
	}
	if d.opts.OnResult != nil {
		d.opts.OnResult(&res)
	}

	out := map[string]any{
		"digest":     res.Digest,
		"raw_bytes":  res.RawBytes,
		"method":     res.Method,
		"latency_ms": latencyMS,
	}
	if res.CallID != "" {
		out["call_id"] = res.CallID
	}
	if len(res.Metadata) > 0 {
		out["digest_meta"] = res.Metadata
	}
	if res.Savings != nil {
		out["savings"] = map[string]any{
			"path":                res.Savings.Path,
			"original_bytes":      res.Savings.OriginalBytes,
			"digest_bytes":        res.Savings.DigestBytes,
			"original_tokens_est": res.Savings.OriginalTokensEst,
			"digest_tokens_est":   res.Savings.DigestTokensEst,
		}
	}
	return out, nil
}

// Unwrap returns the tool this one wraps.
//
// mast calls a tool on its own behalf in exactly one place the model is
// not part of: the write gate's change-set precondition read
// (cmd/mast/toolschemas.go). That caller wants the tool's own bytes.
// Nothing it reads reaches a transcript, so there is nothing to save —
// and a digest envelope carries a fresh call_id and a wall-clock
// latency on every call, which would void an operator's grant on the
// next read of a large enough status.
func (d *digestingTool) Unwrap() tool.Tool { return d.runnableTool }
