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
	"encoding/json"
	"errors"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

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
	scan, _ := got["scan"].(map[string]any)
	if scan == nil || scan["state"] != "steady" {
		t.Errorf("collected[scan] = %v, want the tool's own result carried through whole", got["scan"])
	}
	if _, keyed := got["transitions"]; !keyed {
		t.Errorf("collected = %v, want the diff filed under its `as:` key", got)
	}
	if _, unaliased := got["diff"]; unaliased {
		t.Errorf("collected = %v, want the alias to replace the tool name, not sit beside it", got)
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
	if got != nil {
		t.Errorf("collect returned %v alongside the error; a partial map is the thing that must not reach the model", got)
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
	if err != nil || got != nil {
		t.Errorf("collect = %v, %v; want nil, nil", got, err)
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
	if err != nil || got != nil {
		t.Errorf("collect = %v, %v; want nil, nil", got, err)
	}
}

// TestCollectPassesTheFireIdentity: an MCP server that logs its caller
// should see the cycle's session and an agent name no specialist can
// take, rather than an empty string that reads as an unattributed call.
func TestCollectPassesTheFireIdentity(t *testing.T) {
	rec := &recordingRun{results: map[string]map[string]any{"scan": {}}}
	c := testCollector(t, rec, workload.MonitorCollect{Tool: "scan"})

	if _, err := c.collect(context.Background(), "sched-2026-08-21T10:00:00Z"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	ctx := rec.sawCtx[0]
	if ctx.SessionID() != "sched-2026-08-21T10:00:00Z" {
		t.Errorf("SessionID = %q, want the fire's session", ctx.SessionID())
	}
	if ctx.AppName() != "mast" || ctx.UserID() != "daemon" {
		t.Errorf("app/user = %q/%q, want mast/daemon", ctx.AppName(), ctx.UserID())
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
	if scan, _ := got["scan"].(map[string]any); scan == nil || scan["ok"] != true {
		t.Errorf("collected = %v, want the tool's result", got)
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
