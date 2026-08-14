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

// Package deploy holds the Kubernetes manifests mast ships. It has no
// Go source — only these tests, which guard the two properties of the
// manifests that a change elsewhere in the repo can silently break: the
// deployed bundle is the bundle the repo documents, and every file in
// it reaches the pod.
package deploy

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	// configDir is what the ConfigMap generator reads. kustomize
	// refuses paths outside its own root, so this is a copy of the
	// example bundle rather than a reference to it — hence the drift
	// test below.
	configDir = "base/config"
	bundleDir = "../examples/workloads/gke-triage"

	manifest      = "base/50-statefulset-daemon.yaml"
	kustomization = "base/kustomization.yaml"
)

// TestDeployConfigMatchesTheExampleBundle pins the deployed copy of the
// gke-triage bundle to the example the docs and tests exercise.
//
// They are two directories holding the same files, and only the example
// has tests over it (pkg/specialists' roster test, the UAT harness,
// internal/compose's capability check via a live load). Drift means the
// cluster runs a roster nothing verified. It had already happened once
// before this test existed: at b46e0ba the deployed workload.yaml was
// missing the `hitl:` block the example had carried since spike 2, so
// the manifests deployed an agent with different change-safety
// configuration than the one every doc described. W2.4 made the stakes
// concrete — the drift to catch now is a deployed diagnoser that still
// holds patch_resource after the example's stopped.
func TestDeployConfigMatchesTheExampleBundle(t *testing.T) {
	deployed := treeOf(t, configDir)
	example := treeOf(t, bundleDir)

	for path, want := range example {
		got, ok := deployed[path]
		if !ok {
			t.Errorf("%s is in the example bundle but not in %s — the deployed roster is missing it", path, configDir)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs between %s and %s; copy the example over the deployed one (they are meant to be identical)", path, bundleDir, configDir)
		}
	}
	for path := range deployed {
		if _, ok := example[path]; !ok {
			t.Errorf("%s is in %s but not in the example bundle — nothing tests it", path, configDir)
		}
	}
}

// TestWorkloadProjectionCoversTheBundle pins both enumerations of the
// bundle — the ConfigMap generator's files: list and the StatefulSet's
// items: projection — to the files actually on disk.
//
// The failure this prevents has no other symptom. ConfigMap keys cannot
// contain "/", so the workload dir is flattened into a single map and
// items:.path rebuilds the tree in the pod — and an items: list
// projects only the keys it names. Add a specialist to the roster and
// forget these blocks and the pod comes up healthy, missing that
// specialist, routing its failure mode to _fallback. Add a schema and
// forget it and the specialist that references it fails to load. Both
// are silent downgrades of a running triage agent, which is the class
// of thing nobody notices until an incident lands on the missing
// specialist.
//
// Added 2026-08-14 in W2.4, whose change-executor specialist plus its
// change-report schema were two more chances to hit it.
func TestWorkloadProjectionCoversTheBundle(t *testing.T) {
	onDisk := map[string]bool{}
	for path := range treeOf(t, configDir) {
		onDisk[path] = true
	}

	for _, source := range []struct {
		what  string
		where string
		paths map[string]bool
	}{
		{"the ConfigMap generator", kustomization, generatedPaths(t)},
		{"the pod's volume projection", manifest, projectedPaths(t)},
	} {
		for path := range onDisk {
			if !source.paths[path] {
				t.Errorf("%s is in the bundle but missing from %s — add it to %s", path, source.what, source.where)
			}
		}
		for path := range source.paths {
			if !onDisk[path] {
				t.Errorf("%s names %s, which is not in the bundle — the pod would fail on a missing ConfigMap key", source.what, path)
			}
		}
	}
}

// treeOf reads every file under dir, keyed by slash-separated path
// relative to dir.
func treeOf(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s is empty", dir)
	}
	return out
}

// generatedPaths returns the bundle-relative paths the mast-workload
// ConfigMap generator carries, from its `key=path` entries.
func generatedPaths(t *testing.T) map[string]bool {
	t.Helper()
	var k struct {
		ConfigMapGenerator []struct {
			Name  string   `yaml:"name"`
			Files []string `yaml:"files"`
		} `yaml:"configMapGenerator"`
	}
	unmarshalFile(t, kustomization, &k)

	out := map[string]bool{}
	for _, g := range k.ConfigMapGenerator {
		if g.Name != "mast-workload" {
			continue
		}
		for _, f := range g.Files {
			key, path, ok := strings.Cut(f, "=")
			if !ok {
				t.Errorf("configMapGenerator entry %q is not key=path", f)
				continue
			}
			rel := strings.TrimPrefix(path, "config/")
			checkFlattened(t, key, rel)
			out[rel] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("no mast-workload configMapGenerator in %s", kustomization)
	}
	return out
}

// projectedPaths returns the in-pod paths the `workload` volume mounts.
func projectedPaths(t *testing.T) map[string]bool {
	t.Helper()
	var sts struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						Name      string `yaml:"name"`
						ConfigMap struct {
							Items []struct {
								Key  string `yaml:"key"`
								Path string `yaml:"path"`
							} `yaml:"items"`
						} `yaml:"configMap"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	unmarshalFile(t, manifest, &sts)

	out := map[string]bool{}
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name != "workload" {
			continue
		}
		for _, it := range v.ConfigMap.Items {
			checkFlattened(t, it.Key, it.Path)
			out[it.Path] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("no `workload` volume with a configMap items: list in %s", manifest)
	}
	return out
}

// checkFlattened holds both enumerations to the generator's convention:
// the ConfigMap key is the path with "/" replaced by "_". A key that
// does not match its path still deploys — it just projects the wrong
// file, or none.
func checkFlattened(t *testing.T, key, path string) {
	t.Helper()
	if want := strings.ReplaceAll(path, "/", "_"); key != want {
		t.Errorf("%s is keyed %q, want %q — the generator flattens paths with _", path, key, want)
	}
}

func unmarshalFile(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}
