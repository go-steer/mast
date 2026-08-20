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
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/mast/pkg/observability"
)

// tracer is mast's own instrumentation scope. ADK owns the spans
// beneath a turn — generate_content, execute_tool, invoke_agent,
// invoke_node — from google.golang.org/adk/v2/internal/telemetry,
// which is not importable. Everything mast starts itself goes here.
var tracer = otel.Tracer("mast/cmd")

// turnSpan brackets one agent turn.
//
// It exists because ADK's spans parent off whatever context reaches
// runner.Run, and mast does not always hand it one with a span on it.
// On the request-driven paths — inject, resume, attach, A2A, AG-UI —
// otelhttp has already opened a server span, so ADK's invoke_agent
// lands under it and the tree is intact. On the paths with no request
// behind them — a scheduled trigger firing, an auto-resume, the
// oneshot CLI — the context is a long-lived daemon or process context
// with no span at all, so invoke_agent became a **trace root**: every
// model call, tool call and MCP call in that turn hung off a trace
// that started nowhere, and nothing named the session it belonged to.
//
// Starting our own span on the per-turn context fixes both halves at
// once. ADK becomes our child with no ADK fork and no upstream change,
// and there is finally somewhere to hang the facts an operator needs
// to find the turn again: which session, which workload, what kind of
// turn, how it ended, what it cost.
//
// mast needs no equivalent of upstream's inject→turn span links
// (core-agent a58fdcc). Links are for asynchronous fan-in — upstream's
// inbox batches injects and one turn can answer several, so a parent
// edge would have to pick one. mast dispatches an inject
// **synchronously** on the request goroutine (cmd/mast/main.go's inject
// handler calls dispatch with the request context), so the inject's
// server span is already this span's parent. One inject, one turn, a
// real parent edge.
type turnSpan struct {
	span     trace.Span
	obs      *observability.Registry
	workload string
	started  time.Time

	// reported guards against double-counting: complete() is called on
	// each of the turn loop's exit paths, and end() runs deferred for
	// the paths that return before the loop is reached at all.
	reported bool
}

// startTurnSpan opens the turn's root span and returns the context to
// run the turn under. Called before the turn lock, deliberately: the
// wait to get that lock is part of how long an inject took to answer,
// and a turn refused at the chokepoint — aborted session, gate pause,
// watchdog halt — is exactly the turn an operator goes looking for.
//
// label is the turn label the whole chokepoint already carries, and it
// is split rather than stored whole. Every producer writes it
// `kind:detail` — `inject:<reason>`, `scheduled:<RFC3339>`,
// `autoresume:<session>`, `resume:<interrupt>`, `a2a:message/send` — so
// the part before the colon is a bounded vocabulary worth grouping and
// filtering by, and the part after is one turn's particulars. Stored
// whole, the useful half would be unusable: no backend groups by a
// string with a timestamp in it.
func startTurnSpan(ctx context.Context, obs *observability.Registry, workload, sessionID, label string) (context.Context, *turnSpan) {
	kind, detail, _ := strings.Cut(label, ":")
	attrs := []attribute.KeyValue{
		attribute.String("mast.session.id", sessionID),
		attribute.String("mast.workload.name", workload),
		attribute.String("mast.turn.kind", kind),
	}
	if detail != "" {
		attrs = append(attrs, attribute.String("mast.turn.detail", detail))
	}
	ctx, span := tracer.Start(ctx, "mast.turn",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	return ctx, &turnSpan{span: span, obs: obs, workload: workload, started: time.Now()}
}

// locked stamps how long the turn waited for its session's turn lock.
// One session runs one turn at a time (#62), so on a busy session most
// of an inject's latency can be queueing — which reads as a slow model
// unless the span says otherwise.
func (t *turnSpan) locked() {
	if t == nil {
		return
	}
	t.span.SetAttributes(attribute.Int64("mast.turn.queued_ms", time.Since(t.started).Milliseconds()))
}

// cost stamps the turn's own spend, once the meter delta is known.
// The session-cumulative figure lives in the meter; this is the delta
// this turn added, the same number obs.AddCost receives.
func (t *turnSpan) cost(usd float64) {
	if t == nil {
		return
	}
	t.span.SetAttributes(attribute.Float64("mast.cost.usd", usd))
}

// complete records how the turn ended to the metric registry and to
// the span together, so the two can never disagree about the same
// turn. Every caller that used to call obs.TurnComplete directly calls
// this instead — the outcome vocabulary is observability's, unchanged.
//
// err is the error being returned, or nil. It sets the span status;
// the outcome is what says *which* failure, since a watchdog halt and
// a budget trip are both errors and an operator needs to tell them
// apart without reading the message.
func (t *turnSpan) complete(outcome string, err error) {
	if t == nil {
		return
	}
	t.obs.TurnComplete(t.workload, outcome)
	t.reported = true
	t.span.SetAttributes(attribute.String("mast.turn.outcome", outcome))
	if err != nil {
		t.span.RecordError(err)
		t.span.SetStatus(codes.Error, outcome)
	}
}

// end closes the span. Deferred at the top of the turn so it runs on
// every path, including the chokepoint refusals that return before the
// runner is ever reached.
//
// Those refusals do NOT get an obs.TurnComplete — mast_turns_total has
// only ever counted turns that started, and quietly changing that
// would move every dashboard's denominator. The span still records
// them, under a `refused` outcome that is a span attribute and not a
// metric label: a turn an operator's own abort or gate-pause stopped
// is the first thing they look for in a trace, and it is the one thing
// the metric cannot show them.
func (t *turnSpan) end(err error) {
	if t == nil {
		return
	}
	if !t.reported {
		outcome := "refused"
		if err == nil {
			// No error and no reported outcome means a path returned
			// success without going through the turn loop. Nothing
			// does that today; naming it beats a silently wrong label
			// if something starts to.
			outcome = "unreported"
		}
		t.span.SetAttributes(attribute.String("mast.turn.outcome", outcome))
		if err != nil {
			t.span.RecordError(err)
			t.span.SetStatus(codes.Error, outcome)
		}
	}
	t.span.End()
}
