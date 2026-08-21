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
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/mast/pkg/monitor"
	"github.com/go-steer/mast/pkg/workload"
)

// The collection leg (v0.5 W4.2): what a monitoring cycle gathers on
// mast's own behalf, before the model is woken.

// recordingRun is a stand-in for toolSchemas.collect that records the
// order it was called in. The seam under test is the collector's, and
// toolschemas_test.go already pins the run half of it.
type recordingRun struct {
	saw     []string
	sawArgs []map[string]any
	sawCtx  []adkagent.Context
	results map[string]map[string]any
	fail    map[string]error
}

func (r *recordingRun) run(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error) {
	r.saw = append(r.saw, name)
	r.sawArgs = append(r.sawArgs, args)
	r.sawCtx = append(r.sawCtx, ctx)
	if err, bad := r.fail[name]; bad {
		return nil, err
	}
	return r.results[name], nil
}

func testCollector(t *testing.T, rec *recordingRun, calls ...workload.MonitorCollect) *monitorCollector {
	t.Helper()
	return newMonitorCollector(discardLogger(), workload.Monitor{Collect: calls}, rec.run, "mast", "daemon")
}

// TestCollectRunsInDeclarationOrder: the shape this exists for is a scan
// followed by a diff OF that scan, so the bundle's order is the
// dependency order. Running them concurrently would classify against a
// run that had not finished.
func TestCollectRunsInDeclarationOrder(t *testing.T) {
	rec := &recordingRun{results: map[string]map[string]any{
		"scan": {"state": "steady"},
		"diff": {"new": float64(2)},
	}}
	c := testCollector(t, rec,
		workload.MonitorCollect{Tool: "scan"},
		workload.MonitorCollect{Tool: "diff", As: "transitions", Args: map[string]any{"window": "1h"}},
	)

	got, err := c.collect(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(rec.saw) != 2 || rec.saw[0] != "scan" || rec.saw[1] != "diff" {
		t.Errorf("ran %v, want [scan diff] in declaration order", rec.saw)
	}
	// Keyed by `as:` where given, by tool name where not.
	scan, _ := got.Collected["scan"].(map[string]any)
	if scan == nil || scan["state"] != "steady" {
		t.Errorf("collected[scan] = %v, want the tool's own result carried through whole", got.Collected["scan"])
	}
	if _, keyed := got.Collected["transitions"]; !keyed {
		t.Errorf("collected = %v, want the diff filed under its `as:` key", got.Collected)
	}
	if _, unaliased := got.Collected["diff"]; unaliased {
		t.Errorf("collected = %v, want the alias to replace the tool name, not sit beside it", got.Collected)
	}
	if rec.sawArgs[1]["window"] != "1h" {
		t.Errorf("the diff saw %v, want the bundle's literal arguments", rec.sawArgs[1])
	}
}

// TestCollectAbortsOnTheFirstFailure is the one that has to be argued
// for, because "collect what you can" sounds resilient. It is not: a
// cycle whose diff failed and whose scan succeeded hands the model a
// snapshot with no transitions attached, and the honest reading of "no
// transitions" is "nothing changed". A monitor that reports calm because
// its collection broke is worse than one that reports nothing, because
// only the second is visibly broken.
func TestCollectAbortsOnTheFirstFailure(t *testing.T) {
	rec := &recordingRun{
		results: map[string]map[string]any{"scan": {"state": "steady"}},
		fail:    map[string]error{"diff": errors.New("mcp server gone")},
	}
	c := testCollector(t, rec,
		workload.MonitorCollect{Tool: "scan"},
		workload.MonitorCollect{Tool: "diff"},
		workload.MonitorCollect{Tool: "budget"},
	)

	got, err := c.collect(context.Background(), "sess-1")
	if err == nil {
		t.Fatal("collect succeeded with a failing call; the model would be woken with a partial picture")
	}
	if got.Collected != nil || got.Transitions != nil {
		t.Errorf("collect returned %+v alongside the error; a partial picture is the thing that must not reach the model", got)
	}
	if !strings.Contains(err.Error(), "mcp server gone") {
		t.Errorf("error = %v, want the tool's own failure carried up", err)
	}
	// Nothing after the failure runs: the cycle is over, and a call that
	// advances persisted state must not fire for a cycle nobody will act
	// on.
	if len(rec.saw) != 2 {
		t.Errorf("ran %v, want the run to stop at the failing call", rec.saw)
	}
}

// TestCollectStopsOnACancelledContext: the fire's context carries the
// budget's wallclock ceiling, so a wedged server has to end the cycle
// rather than hold it past the next tick.
func TestCollectStopsOnACancelledContext(t *testing.T) {
	rec := &recordingRun{results: map[string]map[string]any{}}
	c := testCollector(t, rec,
		workload.MonitorCollect{Tool: "scan"},
		workload.MonitorCollect{Tool: "diff"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.collect(ctx, "sess-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("collect err = %v, want context.Canceled", err)
	}
	if len(rec.saw) != 0 {
		t.Errorf("ran %v on a cancelled context, want nothing", rec.saw)
	}
}

// TestCollectIsANoOpWithoutAMonitorBlock: the overwhelming majority of
// workloads. A nil map here is what keeps `collected` out of the
// envelope entirely, rather than putting an empty object in front of
// every scheduled model on every fire.
func TestCollectIsANoOpWithoutAMonitorBlock(t *testing.T) {
	rec := &recordingRun{}
	c := newMonitorCollector(discardLogger(), workload.Monitor{}, rec.run, "mast", "daemon")

	if c.enabled() {
		t.Error("a collector with no calls reports enabled")
	}
	got, err := c.collect(context.Background(), "sess-1")
	if err != nil || got.Collected != nil || got.Transitions != nil {
		t.Errorf("collect = %+v, %v; want the zero cycle and no error", got, err)
	}
	if len(rec.saw) != 0 {
		t.Errorf("ran %v, want nothing", rec.saw)
	}
}

// A nil collector is the same no-op, so the fire callback does not have
// to branch on both "no block" and "no collector".
func TestNilCollectorIsSafe(t *testing.T) {
	var c *monitorCollector
	if c.enabled() {
		t.Error("a nil collector reports enabled")
	}
	got, err := c.collect(context.Background(), "sess-1")
	if err != nil || got.Collected != nil || got.Transitions != nil {
		t.Errorf("collect = %+v, %v; want the zero cycle and no error", got, err)
	}
}

// TestCollectPassesTheFireIdentity: an MCP server that logs its caller
// should see the cycle's session and an agent name no specialist can
// take, rather than an empty string that reads as an unattributed call.
func TestCollectPassesTheFireIdentity(t *testing.T) {
	rec := &recordingRun{results: map[string]map[string]any{"scan": {}}}
	// Built directly rather than through testCollector, with an app and
	// user that are deliberately not the daemon's: the claim is that the
	// cycle runs under the identity it was handed, which a collector
	// hardcoding the daemon's own constants would also pass.
	c := newMonitorCollector(discardLogger(),
		workload.Monitor{Collect: []workload.MonitorCollect{{Tool: "scan"}}},
		rec.run, "mast-under-test", "daemon")

	if _, err := c.collect(context.Background(), "sched-2026-08-21T10:00:00Z"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	ctx := rec.sawCtx[0]
	if ctx.SessionID() != "sched-2026-08-21T10:00:00Z" {
		t.Errorf("SessionID = %q, want the fire's session", ctx.SessionID())
	}
	if ctx.AppName() != "mast-under-test" || ctx.UserID() != "daemon" {
		t.Errorf("app/user = %q/%q, want the identity the collector was built with", ctx.AppName(), ctx.UserID())
	}
	if ctx.AgentName() != "mast:monitor" {
		t.Errorf("AgentName = %q, want the namespaced form no specialist name can take", ctx.AgentName())
	}
}

// contextReadingTool asserts the collection context is usable by a real
// ADK tool — not just that it compiles, but that the methods an
// mcptoolset-wrapped tool actually touches answer without panicking.
type contextReadingTool struct {
	catalogTool
	sawAgent   string
	sawSession string
	confirmErr error
	stateErr   error
}

func (c *contextReadingTool) Run(ctx adkagent.Context, _ any) (map[string]any, error) {
	c.sawAgent = ctx.AgentName()
	c.sawSession = ctx.SessionID()
	// The two ADK calls mcptoolset makes on this path. Neither may
	// panic; ToolConfirmation being nil is the normal answer, and
	// Actions has to be a real struct because a tool that writes a
	// state delta must not nil-deref.
	_ = ctx.ToolConfirmation()
	ctx.Actions().StateDelta["seen"] = true
	c.confirmErr = ctx.RequestConfirmation("approve?", nil)
	c.stateErr = ctx.State().Set("k", "v")
	return map[string]any{"ok": true}, nil
}

// TestCollectContextDrivesARealTool: the end of the seam. The collector
// hands its context to toolSchemas.collect, which asserts the runnable
// handle and calls Run — the same path a live MCP tool takes.
func TestCollectContextDrivesARealTool(t *testing.T) {
	probe := &contextReadingTool{catalogTool: catalogTool{name: "scan", desc: "scan"}}
	ts := testSchemas(&catalogToolset{name: "gke", tools: []tool.Tool{probe}})
	rec := &monitorCollector{
		calls:   []workload.MonitorCollect{{Tool: "scan"}},
		run:     ts.collect,
		logger:  discardLogger(),
		appName: "mast",
		userID:  "daemon",
	}

	got, err := rec.collect(context.Background(), "sess-9")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if scan, _ := got.Collected["scan"].(map[string]any); scan == nil || scan["ok"] != true {
		t.Errorf("collected = %v, want the tool's result", got.Collected)
	}
	if probe.sawAgent != "mast:monitor" || probe.sawSession != "sess-9" {
		t.Errorf("the tool saw %q/%q, want mast:monitor/sess-9", probe.sawAgent, probe.sawSession)
	}
	// Both refusals are errors rather than nils. A collection call that
	// parked for a human would park forever — nobody is awake at 3am and
	// there is no invocation to resume into — and a state write that
	// silently succeeded would be a write into a session that does not
	// exist yet.
	if probe.confirmErr == nil {
		t.Error("RequestConfirmation succeeded; a collection call has no invocation to park on")
	}
	if probe.stateErr == nil {
		t.Error("State().Set succeeded; there is no session for a collection call to write into")
	}
}

// TestCollectRefusesAToolItCannotRun: the same "silence and nothing
// changed must never be confused" rule the precondition read has, in the
// direction that matters here — a collection that read as empty is a
// monitor that has stopped monitoring.
func TestCollectRefusesAToolItCannotRun(t *testing.T) {
	ts := testSchemas(&catalogToolset{
		name:  "gke",
		tools: []tool.Tool{catalogTool{name: "scan", desc: "not runnable"}},
	})

	_, err := ts.collect(nil, "scan", nil)
	if err == nil {
		t.Fatal("a tool mast cannot run came back as a successful empty collection")
	}
	if !strings.Contains(err.Error(), "monitor collection scan") {
		t.Errorf("error = %v, want the collection leg named, not the precondition read's wording", err)
	}
	if !strings.Contains(err.Error(), "monitor.collect call") {
		t.Errorf("error = %v, want it to say which exception could not be served", err)
	}
}

// TestCollectedRidesInTheEnvelope: the wake-up the model actually reads.
// One JSON object, so the facts and the tick they were sampled at cannot
// be separated by a resume.
func TestCollectedRidesInTheEnvelope(t *testing.T) {
	body, err := json.Marshal(scheduledPayload{
		Kind:      "scheduled",
		Workload:  "watch",
		Tick:      "2026-08-21T10:00:00Z",
		Collected: map[string]any{"transitions": map[string]any{"new": 2}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, _ := back["collected"].(map[string]any)
	if got == nil {
		t.Fatalf("envelope = %s, want a collected object", body)
	}
	tr, _ := got["transitions"].(map[string]any)
	if tr == nil || tr["new"] != float64(2) {
		t.Errorf("collected = %v, want the tool's result passed through unreshaped", got)
	}
	if back["tick"] != "2026-08-21T10:00:00Z" {
		t.Errorf("tick = %v, want the same envelope to carry which fire these facts belong to", back["tick"])
	}
}

// The W4.4 half: the cycle reads the classification, and reads nothing
// into it.

// diffOut is what a lookout-shaped tool hands back through ADK's MCP
// adapter: text under an `output` key, one record per line, terminated
// by the summary.
func diffOut(lines ...string) map[string]any {
	return map[string]any{"output": strings.Join(lines, "\n") + "\n"}
}

func transitionCollector(t *testing.T, rec *recordingRun, key string, calls ...workload.MonitorCollect) *monitorCollector {
	t.Helper()
	return newMonitorCollector(discardLogger(),
		workload.Monitor{Collect: calls, TransitionsFrom: key},
		rec.run, "mast", "daemon")
}

// TestCollectParsesTheNamedTransitionSource: the source named by
// monitor.transitions_from comes back parsed, and does NOT also come
// back raw. Two spellings of one answer in one envelope is an invitation
// to reconcile them.
func TestCollectParsesTheNamedTransitionSource(t *testing.T) {
	rec := &recordingRun{results: map[string]map[string]any{
		"scan": {"nodes": float64(12)},
		"diff": diffOut(
			`transition=new subject_key=pod/prod/api/CrashLoopBackOff severity=critical`,
			`transition=resolved subject_key=pod/prod/web/Unhealthy severity=warning`,
			`scanned=412 findings=2 elapsed=1.9s`,
		),
	}}
	c := transitionCollector(t, rec, "transitions",
		workload.MonitorCollect{Tool: "scan"},
		workload.MonitorCollect{Tool: "diff", As: "transitions"},
	)

	got, err := c.collect(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got.Transitions == nil {
		t.Fatal("Transitions = nil for a workload that named a source")
	}
	if n := len(got.Transitions.Transitions); n != 2 {
		t.Fatalf("got %d transitions, want 2: %+v", n, got.Transitions)
	}
	if got.Transitions.Scanned != 412 {
		t.Errorf("Scanned = %d, want 412", got.Transitions.Scanned)
	}
	if _, raw := got.Collected["transitions"]; raw {
		t.Errorf("collected = %v; the parsed set must not be shadowed by the text it was read from", got.Collected)
	}
	// Everything else still rides raw. Naming one source classifies one
	// result, not the whole block.
	if scan, _ := got.Collected["scan"].(map[string]any); scan == nil || scan["nodes"] != float64(12) {
		t.Errorf("collected[scan] = %v, want the other calls untouched", got.Collected["scan"])
	}
}

// TestCollectDoesNotSecondGuessTheClassification is the runtime end of
// the leg pkg/monitor pins: the stub reports `escalated` for a subject
// whose severity mast can see did not change, and mast reports it as
// escalated anyway. This is the test that fails the moment anyone adds
// a local heuristic — anywhere between the tool result and the envelope.
func TestCollectDoesNotSecondGuessTheClassification(t *testing.T) {
	rec := &recordingRun{results: map[string]map[string]any{
		"diff": diffOut(
			`transition=escalated subject_key=pod/prod/api/Unhealthy severity=warning prev_severity=warning`,
			`scanned=1 findings=1`,
		),
	}}
	c := transitionCollector(t, rec, "diff", workload.MonitorCollect{Tool: "diff"})

	got, err := c.collect(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got.Transitions == nil || len(got.Transitions.Transitions) != 1 {
		t.Fatalf("transitions = %+v, want the one record through", got.Transitions)
	}
	if class := got.Transitions.Transitions[0].Class; class != "escalated" {
		t.Errorf("class = %q, want escalated — the severities being equal is lookout's business, not mast's", class)
	}
}

// TestCollectAbortsOnAMalformedClassification: the strictness argument.
// A truncated diff read leniently becomes an empty transition set, and
// an empty transition set is the wire for "all quiet" — which W4.5 will
// decline to notify on. So it fails the cycle instead, and the failure
// names the bundle key, the tool, and what was wrong with the bytes.
func TestCollectAbortsOnAMalformedClassification(t *testing.T) {
	rec := &recordingRun{results: map[string]map[string]any{
		"scan": {"nodes": float64(12)},
		// A prefix of a healthy answer: records, no summary line.
		"diff": diffOut(`transition=new subject_key=pod/prod/api/CrashLoopBackOff`),
	}}
	c := transitionCollector(t, rec, "transitions",
		workload.MonitorCollect{Tool: "scan"},
		workload.MonitorCollect{Tool: "diff", As: "transitions"},
	)

	got, err := c.collect(context.Background(), "sess-1")
	if err == nil {
		t.Fatalf("collect accepted a truncated diff: %+v", got)
	}
	if got.Collected != nil || got.Transitions != nil {
		t.Errorf("collect returned %+v alongside the error", got)
	}
	for _, want := range []string{`"transitions"`, `"diff"`, "summary line"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %s", err, want)
		}
	}
}

// A tool that answers with structured content is not a transition
// source, and saying so beats coercing a shape with no terminator in it.
func TestCollectRefusesAStructuredTransitionSource(t *testing.T) {
	// ADK's mcptoolset files a server's STRUCTURED content under the
	// same `output` key it files text under, so this is what a tool
	// answering in JSON objects actually looks like from here.
	rec := &recordingRun{results: map[string]map[string]any{
		"diff": {"output": map[string]any{"findings": []any{}, "scanned": float64(3)}},
	}}
	c := transitionCollector(t, rec, "diff", workload.MonitorCollect{Tool: "diff"})

	if _, err := c.collect(context.Background(), "sess-1"); err == nil {
		t.Fatal("collect accepted a structured result as a transition source")
	} else if !strings.Contains(err.Error(), "record stream") {
		t.Errorf("error = %v, want it to say what the contract is", err)
	}
}

// A quiet cycle is a SUCCESS with an empty set — distinct from a
// workload that classifies nothing (nil), and the distinction is what
// W4.5 decides from.
func TestCollectQuietCycleIsEmptyNotAbsent(t *testing.T) {
	rec := &recordingRun{results: map[string]map[string]any{
		"diff": diffOut(`scanned=412 findings=0 elapsed=1.1s`),
	}}
	c := transitionCollector(t, rec, "diff", workload.MonitorCollect{Tool: "diff"})

	got, err := c.collect(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got.Transitions == nil {
		t.Fatal("Transitions = nil on a quiet cycle; nil means the workload does not classify at all")
	}
	if !got.Transitions.Empty() {
		t.Errorf("transitions = %+v, want empty", got.Transitions)
	}
	if got.Transitions.Scanned != 412 {
		t.Errorf("Scanned = %d; a cycle that changed nothing and scanned nothing is a broken monitor, and the number is how you tell", got.Transitions.Scanned)
	}
}

// A workload that collects without naming a source keeps every result
// raw. Classification is opt-in.
func TestCollectWithoutATransitionSourceLeavesResultsRaw(t *testing.T) {
	rec := &recordingRun{results: map[string]map[string]any{
		"diff": diffOut(`transition=new subject_key=a`, `scanned=1 findings=1`),
	}}
	c := testCollector(t, rec, workload.MonitorCollect{Tool: "diff"})

	got, err := c.collect(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got.Transitions != nil {
		t.Errorf("Transitions = %+v for a workload that named no source", got.Transitions)
	}
	if _, raw := got.Collected["diff"]; !raw {
		t.Errorf("collected = %v, want the result carried through raw", got.Collected)
	}
}

// TestCollectLogsTheTallyItObserved: the operator-facing line. Built
// from the classes that turned up rather than from a list mast keeps, so
// a class lookout ships tomorrow is counted correctly by a build from
// today.
func TestCollectLogsTheTallyItObserved(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingRun{results: map[string]map[string]any{
		"diff": diffOut(
			`transition=new subject_key=a`,
			`transition=quiesced subject_key=b`,
			`transition=new subject_key=c`,
			`scanned=40 findings=3`,
		),
	}}
	c := newMonitorCollector(slog.New(slog.NewJSONHandler(&out, nil)),
		workload.Monitor{Collect: []workload.MonitorCollect{{Tool: "diff"}}, TransitionsFrom: "diff"},
		rec.run, "mast", "daemon")

	if _, err := c.collect(context.Background(), "sess-7"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	line := out.String()
	if !strings.Contains(line, "monitoring cycle classified what changed") {
		t.Fatalf("logs = %s, want the classification line", line)
	}
	for _, want := range []string{`"scanned":40`, `"transitions":3`, `"new=2"`, `"quiesced=1"`, `"session":"sess-7"`} {
		if !strings.Contains(line, want) {
			t.Errorf("logs = %s, want %s", line, want)
		}
	}
}

// TestTransitionsRideInTheEnvelope: the wake-up the model actually
// reads. `transitions.records` is always present when a source was
// named — an explicit empty list is "nothing changed", and a missing key
// is "we do not know".
func TestTransitionsRideInTheEnvelope(t *testing.T) {
	set, err := monitor.Parse("transition=new subject_key=pod/prod/api/CrashLoop severity=critical\nscanned=9 findings=1\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := json.Marshal(scheduledPayload{
		Kind:        "scheduled",
		Workload:    "watch",
		Tick:        "2026-08-21T10:00:00Z",
		Collected:   map[string]any{"health": map[string]any{"nodes": 12}},
		Transitions: &set,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tr, _ := back["transitions"].(map[string]any)
	if tr == nil {
		t.Fatalf("envelope = %s, want a transitions object", body)
	}
	if tr["scanned"] != float64(9) {
		t.Errorf("scanned = %v, want 9", tr["scanned"])
	}
	records, _ := tr["records"].([]any)
	if len(records) != 1 {
		t.Fatalf("records = %v, want one", tr["records"])
	}
	rec0, _ := records[0].(map[string]any)
	if rec0["transition"] != "new" {
		t.Errorf("record = %v, want the producer's own spelling of the class", rec0)
	}
	if rec0["subject_key"] != "pod/prod/api/CrashLoop" {
		t.Errorf("record = %v, want the subject key an ack will be keyed by", rec0)
	}
	fields, _ := rec0["fields"].(map[string]any)
	if fields["severity"] != "critical" {
		t.Errorf("fields = %v, want the rest of the record carried whole", fields)
	}
	// The tick still says which fire these belong to, and the other
	// collected facts still ride beside them.
	if back["tick"] != "2026-08-21T10:00:00Z" {
		t.Errorf("tick = %v", back["tick"])
	}
	if _, ok := back["collected"].(map[string]any); !ok {
		t.Errorf("envelope = %s, want the unclassified facts alongside", body)
	}
}

// A quiet cycle still puts an empty list in the envelope. The model is
// being told "we looked and nothing changed", which is a different
// sentence from silence.
func TestQuietCycleSaysSoInTheEnvelope(t *testing.T) {
	set, err := monitor.Parse("scanned=412 findings=0\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := json.Marshal(scheduledPayload{Kind: "scheduled", Workload: "w", Tick: "t", Transitions: &set})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"records":[]`) {
		t.Errorf("envelope = %s, want an explicit empty record list", body)
	}
	if strings.Contains(string(body), "collected") {
		t.Errorf("envelope = %s, want no collected key when the only call was the classification", body)
	}
}

// A workload that classifies nothing carries no transitions key at all,
// rather than an empty set that would read as "we checked".
func TestNoTransitionSourceLeavesTheEnvelopeUnchanged(t *testing.T) {
	body, err := json.Marshal(scheduledPayload{
		Kind: "scheduled", Workload: "w", Tick: "t",
		Collected: map[string]any{"health": map[string]any{}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "transitions") {
		t.Errorf("envelope = %s, want no transitions key on a workload that names no source", body)
	}
}

// A workload with no monitor block puts no `collected` key in front of
// its model at all, rather than an empty object that reads as "we looked
// and found nothing".
func TestNoCollectionLeavesTheEnvelopeUnchanged(t *testing.T) {
	body, err := json.Marshal(scheduledPayload{Kind: "scheduled", Workload: "w", Tick: "t"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "collected") {
		t.Errorf("envelope = %s, want no collected key on a workload that collects nothing", body)
	}
}
