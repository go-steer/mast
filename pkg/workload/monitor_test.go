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

package workload_test

import (
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/workload"
)

// The bundle half of W4.2: what a workload declares so that a monitoring
// cycle can gather its own facts before the model is woken.

func TestLoad_MonitorCollect(t *testing.T) {
	path := writeBundle(t, "b.yaml", `name: x
specialists: [a]
edge_trigger:
  scheduled:
    interval: 15m
monitor:
  collect:
    - tool: k8s_cluster_health
      as: health
    - tool: k8s_findings_diff
      args: {transitions: "new,escalated,resolved"}
      as: transitions
`)
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !b.Monitor.Enabled() {
		t.Fatal("Monitor.Enabled() = false, want true for a bundle that declares two calls")
	}
	if len(b.Monitor.Collect) != 2 {
		t.Fatalf("collect = %+v, want two calls", b.Monitor.Collect)
	}
	// Order is load-bearing: the diff classifies the scan, so it has to
	// run after it. A map-shaped block would have lost this.
	if got := b.Monitor.Collect[0].Tool; got != "k8s_cluster_health" {
		t.Errorf("collect[0] = %q, want the scan first, as declared", got)
	}
	if got := b.Monitor.Collect[1].Args["transitions"]; got != "new,escalated,resolved" {
		t.Errorf("collect[1].args = %v, want the literal arguments carried through", b.Monitor.Collect[1].Args)
	}
	if got := b.Monitor.CollectTools(); len(got) != 2 || got[0] != "k8s_cluster_health" || got[1] != "k8s_findings_diff" {
		t.Errorf("CollectTools() = %v, want both tools in declaration order", got)
	}
}

// TestMonitorKeyDefaultsToTheToolName: `as:` exists for the workload
// that collects from one tool twice, and until then the tool name is
// the key an operator would guess.
func TestMonitorKeyDefaultsToTheToolName(t *testing.T) {
	if got := (workload.MonitorCollect{Tool: "k8s_findings_diff"}).Key(); got != "k8s_findings_diff" {
		t.Errorf("Key() = %q, want the tool name", got)
	}
	if got := (workload.MonitorCollect{Tool: "t", As: "transitions"}).Key(); got != "transitions" {
		t.Errorf("Key() = %q, want the declared alias", got)
	}
	// Whitespace is not an alias. An `as: "  "` that survived would file
	// a result under a key nothing can name.
	if got := (workload.MonitorCollect{Tool: "t", As: "   "}).Key(); got != "t" {
		t.Errorf("Key() = %q, want a blank alias to fall back to the tool name", got)
	}
}

// TestMonitorCollectToolsDedupes: the set internal/compose keeps out of
// every roster is a set. A workload that collects from one tool twice
// with different arguments must not produce a refusal message naming it
// twice.
func TestMonitorCollectToolsDedupes(t *testing.T) {
	m := workload.Monitor{Collect: []workload.MonitorCollect{
		{Tool: "diff", As: "a"},
		{Tool: "diff", As: "b"},
		{Tool: "scan"},
	}}
	got := m.CollectTools()
	if len(got) != 2 || got[0] != "diff" || got[1] != "scan" {
		t.Errorf("CollectTools() = %v, want [diff scan]", got)
	}
}

// TestMonitorZeroValueIsNotEnabled: the overwhelming majority of
// bundles declare no monitor block, and every seam that consumes one is
// nil-safe on the zero value rather than guarded at each call site.
func TestMonitorZeroValueIsNotEnabled(t *testing.T) {
	var m workload.Monitor
	if m.Enabled() {
		t.Error("the zero Monitor reports Enabled")
	}
	if got := m.CollectTools(); len(got) != 0 {
		t.Errorf("CollectTools() = %v, want empty", got)
	}
}

func TestLoad_NoMonitorBlockIsEmpty(t *testing.T) {
	path := writeBundle(t, "b.yaml", "name: x\nspecialists: [a]\n")
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if b.Monitor.Enabled() {
		t.Errorf("Monitor = %+v, want the zero value for a bundle that declares none", b.Monitor)
	}
}

// TestLoad_MonitorErrors: what the bundle alone can catch. Whether a
// named tool is wired is checked where the toolsets are, and whether it
// has leaked into a roster is checked where the roster is; these three
// are readable off the YAML.
func TestLoad_MonitorErrors(t *testing.T) {
	for _, tc := range []struct{ name, body, wantErr string }{
		{
			"no tool",
			"name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: 15m\nmonitor:\n  collect:\n    - as: health\n",
			"names no tool",
		},
		{
			// Two results under one key means one silently overwrites
			// the other, and the model reasons over whichever won.
			"duplicate key",
			"name: x\nspecialists: [a]\nedge_trigger:\n  scheduled:\n    interval: 15m\nmonitor:\n  collect:\n    - tool: diff\n      args: {window: 1h}\n    - tool: diff\n      args: {window: 24h}\n",
			"files two results under \"diff\"",
		},
		{
			// A collection block with nothing to trigger it is a block
			// an operator believes is running and that has never run.
			"no cadence to run it",
			"name: x\nspecialists: [a]\nmonitor:\n  collect:\n    - tool: diff\n",
			"declares no edge_trigger.scheduled block",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeBundle(t, "b.yaml", tc.body)
			_, err := workload.Load(path)
			if err == nil {
				t.Fatalf("Load accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// The W4.4 half: which collected result carries the run-to-run
// classification.

func TestLoad_MonitorTransitionsFrom(t *testing.T) {
	path := writeBundle(t, "b.yaml", `name: x
specialists: [a]
edge_trigger:
  scheduled:
    interval: 15m
monitor:
  collect:
    - tool: k8s_cluster_health
      as: health
    - tool: k8s_findings_diff
      args: {transitions: "new,escalated,resolved"}
      as: transitions
  transitions_from: transitions
`)
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := b.Monitor.TransitionsKey(); got != "transitions" {
		t.Errorf("TransitionsKey() = %q, want transitions", got)
	}
}

// TestLoad_MonitorTransitionsFromMustNameACollectedResult: naming a key
// nothing files is a workload that believes it is watching for change
// and is not. It would fail on the first fire regardless; failing at
// load names the typo and lists what was actually collected.
func TestLoad_MonitorTransitionsFromMustNameACollectedResult(t *testing.T) {
	path := writeBundle(t, "b.yaml", `name: x
specialists: [a]
edge_trigger:
  scheduled:
    interval: 15m
monitor:
  collect:
    - tool: k8s_cluster_health
      as: health
    - tool: k8s_findings_diff
      as: diff
  transitions_from: transitions
`)
	_, err := workload.Load(path)
	if err == nil {
		t.Fatal("Load accepted a transitions_from naming nothing collected")
	}
	if !strings.Contains(err.Error(), `names "transitions"`) {
		t.Errorf("error = %v, want it to name the missing key", err)
	}
	// And it says what IS there, so the fix does not need a second
	// look at the file.
	if !strings.Contains(err.Error(), "collected: diff, health") {
		t.Errorf("error = %v, want it to list the collected keys", err)
	}
}

// The default key is the tool name, so a workload that collects one
// tool without an alias can point at it by tool name.
func TestLoad_MonitorTransitionsFromDefaultKey(t *testing.T) {
	path := writeBundle(t, "b.yaml", `name: x
specialists: [a]
edge_trigger:
  scheduled:
    interval: 15m
monitor:
  collect:
    - tool: k8s_findings_diff
  transitions_from: k8s_findings_diff
`)
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := b.Monitor.TransitionsKey(); got != "k8s_findings_diff" {
		t.Errorf("TransitionsKey() = %q", got)
	}
}

// Collecting raw facts and letting the model read them is a supported
// shape. transitions_from is what a workload adds when it wants mast to
// know whether anything changed — it is not a requirement for
// collecting.
func TestLoad_MonitorTransitionsFromIsOptional(t *testing.T) {
	path := writeBundle(t, "b.yaml", `name: x
specialists: [a]
edge_trigger:
  scheduled:
    interval: 15m
monitor:
  collect:
    - tool: k8s_cluster_health
`)
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := b.Monitor.TransitionsKey(); got != "" {
		t.Errorf("TransitionsKey() = %q, want empty", got)
	}
}

// TestLoad_MonitorAliasesDoNotCollide: two calls to the same tool are
// fine as long as they are filed apart, which is what `as:` is for.
func TestLoad_MonitorAliasesDoNotCollide(t *testing.T) {
	path := writeBundle(t, "b.yaml", `name: x
specialists: [a]
edge_trigger:
  scheduled:
    interval: 15m
monitor:
  collect:
    - tool: diff
      args: {window: 1h}
      as: recent
    - tool: diff
      args: {window: 24h}
      as: daily
`)
	b, err := workload.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if b.Monitor.Collect[0].Key() != "recent" || b.Monitor.Collect[1].Key() != "daily" {
		t.Errorf("keys = %q/%q, want recent/daily", b.Monitor.Collect[0].Key(), b.Monitor.Collect[1].Key())
	}
}
