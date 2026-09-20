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

package charts

import (
	"bytes"
	"sort"
	"strings"
	"testing"
)

const (
	// filesDir is the chart's copy of the workload bundle. Helm, like
	// kustomize, refuses to read files outside the chart root, so this
	// is a copy rather than a reference — hence the drift test below.
	filesDir  = "mast/files"
	bundleDir = "../examples/workloads/gke-triage"
)

// TestChartFilesMatchTheExampleBundle pins the deployed copy of the
// gke-triage bundle to the example the docs and tests exercise.
//
// They are two directories holding the same files, and only the example
// has tests over it (pkg/specialists' roster test, the UAT harness,
// internal/compose's capability check via a live load). Drift means the
// cluster runs a roster nothing verified. It had already happened once
// before this test existed: at b46e0ba the deployed workload.yaml was
// missing the `hitl:` block the example had carried since spike 2, so
// the manifests deployed an agent with different change-safety
// configuration than the one every doc described.
func TestChartFilesMatchTheExampleBundle(t *testing.T) {
	deployed := treeOf(t, filesDir)
	example := treeOf(t, bundleDir)

	for path, want := range example {
		got, ok := deployed[path]
		if !ok {
			t.Errorf("%s is in the example bundle but not in %s — the deployed roster is missing it", path, filesDir)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs between %s and %s; copy the example over the deployed one (they are meant to be identical)", path, bundleDir, filesDir)
		}
	}
	for path := range deployed {
		if _, ok := example[path]; !ok {
			t.Errorf("%s is in %s but not in the example bundle — nothing tests it", path, filesDir)
		}
	}
}

// TestWorkloadProjectionCoversTheBundle pins what actually reaches the
// pod to what is on disk.
//
// ConfigMap keys cannot contain "/", so the workload dir is carried
// flat and the volume's items: list rebuilds the tree — and an items:
// list projects only the keys it names. A specialist that is in the
// ConfigMap and missing from the projection is a pod that comes up
// healthy with that failure mode silently routed to _fallback; a schema
// that is missing is a specialist that fails to load.
//
// Under the kustomize base those were two hand-maintained lists and
// this test was the only thing holding them together. In the chart both
// derive from one glob, so the test is now checking that the glob and
// the round trip work rather than that two humans stayed in sync — a
// weaker claim about a stronger arrangement. It stays because the
// failure it catches is still silent.
func TestWorkloadProjectionCoversTheBundle(t *testing.T) {
	onDisk := map[string]bool{}
	for path := range treeOf(t, filesDir) {
		onDisk[path] = true
	}

	docs := defaultRender(t)

	var keys map[string]bool
	for _, d := range docs {
		if d.Kind == "ConfigMap" && d.Metadata.Name == "mast-workload" {
			keys = map[string]bool{}
			for k := range d.Data {
				keys[k] = true
			}
		}
	}
	if keys == nil {
		t.Fatalf("the chart rendered no mast-workload ConfigMap")
	}

	projected := map[string]string{} // in-pod path -> ConfigMap key
	for _, d := range docs {
		if d.Kind != "StatefulSet" {
			continue
		}
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Name != "workload" {
				continue
			}
			for _, it := range v.ConfigMap.Items {
				projected[it.Path] = it.Key
			}
		}
	}
	if len(projected) == 0 {
		t.Fatalf("the StatefulSet mounts no `workload` volume with a configMap items: list")
	}

	for path := range onDisk {
		key, ok := projected[path]
		if !ok {
			t.Errorf("%s is in the bundle but the pod does not project it", path)
			continue
		}
		if want := strings.ReplaceAll(path, "/", "_"); key != want {
			t.Errorf("%s is projected from key %q, want %q — the chart flattens paths with _", path, key, want)
		}
		if !keys[key] {
			t.Errorf("the pod projects key %q, which the ConfigMap does not carry — the mount would fail", key)
		}
	}
	for path := range projected {
		if !onDisk[path] {
			t.Errorf("the pod projects %s, which is not in the bundle", path)
		}
	}
	// And the other direction, from the ConfigMap's keys. Reversing the
	// flattening would be wrong — `specialists__fallback.specialist.md`
	// has an underscore that is part of the filename — so the projected
	// keys are collected instead.
	reached := map[string]bool{}
	for _, key := range projected {
		reached[key] = true
	}
	for key := range keys {
		if !reached[key] {
			t.Errorf("the ConfigMap carries key %q that no volume item projects — it never reaches the pod", key)
		}
	}
}

// TestChartRendersEveryObject is the analogue of "every manifest is
// listed in resources:".
//
// Helm renders every file under templates/, so the kustomize failure —
// a manifest in the directory and missing from the list, which reviews
// as shipped and deploys as absent — cannot happen in that form. What
// can happen is a template that renders to nothing because a condition
// no default sets guards the whole file. For an RBAC object that is the
// same outcome: the grant silently does not exist.
//
// So the set is pinned, and adding an object to the chart means saying
// so here.
func TestChartRendersEveryObject(t *testing.T) {
	want := []string{
		"ClusterRole/k8s-event-watcher",
		"ClusterRole/mast-daemon-read",
		"ClusterRoleBinding/k8s-event-watcher",
		"ClusterRoleBinding/mast-daemon-read",
		"ConfigMap/mast-gcp-env",
		"ConfigMap/mast-workload",
		"Deployment/k8s-event-watcher",
		"Service/mast",
		"ServiceAccount/k8s-event-watcher",
		"ServiceAccount/mast-daemon",
		"StatefulSet/mast",
	}

	var got []string
	for _, d := range defaultRender(t) {
		got = append(got, d.Kind+"/"+d.Metadata.Name)
	}
	sort.Strings(got)

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("a default install renders:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}
