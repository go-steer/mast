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

// Package charts holds the Helm chart mast ships. It has no Go source —
// only these tests, which guard the properties of the deployed bundle
// that a change elsewhere in the repo can silently break: the RBAC
// read/write split, the two usernames every daemon binding has to name,
// the fact that every file in the workload bundle reaches the pod, and
// that the install instructions in the docs still describe this chart
// (installpage_test.go).
//
// They assert against `helm template` output rather than against the
// template files, because a template file is not what gets applied.
package charts

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	chartDir = "mast"
	// releaseNS is the namespace these tests render into. The chart
	// itself has no opinion — it renders into .Release.Namespace — but
	// the WIF subject embeds it, so the tests need a fixed one.
	releaseNS = "mast-triage"

	daemonSA = "mast-daemon"
	// projectID is a stand-in for the operator's. Any value works; what
	// the tests check is that it reaches the places it has to reach.
	projectID = "test-project"

	// wifUser is the RBAC username the API server gives the daemon's
	// Workload Identity Federation principal, for the render below. It
	// is a *User*, not a ServiceAccount, and that is the whole point:
	// see TestBindingsNameTheMCPPathSubject.
	wifUser = "serviceAccount:" + projectID + ".svc.id.goog[" + releaseNS + "/" + daemonSA + "]"

	// skipEnv turns these tests off for a checkout with no helm. It is
	// deliberately noisy to set and nothing in CI sets it: the default
	// when helm is missing is a FAILURE, not a skip, because an RBAC
	// test that quietly skips is indistinguishable from no RBAC test.
	skipEnv = "MAST_SKIP_CHART_TESTS"
)

// doc is the subset of a rendered object these tests reason about.
type doc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Subjects []subject         `yaml:"subjects"`
	Data     map[string]string `yaml:"data"`
	Spec     struct {
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

type subject struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// id is what the failure messages name an object by.
func (d doc) id() string {
	if d.Metadata.Namespace != "" {
		return d.Kind + " " + d.Metadata.Namespace + "/" + d.Metadata.Name
	}
	return d.Kind + " " + d.Metadata.Name
}

// render runs `helm template` and decodes every document it produces.
// Extra arguments are appended, so a test can vary the values.
func render(t *testing.T, extra ...string) []doc {
	t.Helper()
	out, err := helmTemplate(t, extra...)
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", extra, err, out)
	}

	var docs []doc
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var d doc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse rendered output: %v", err)
		}
		if d.Kind == "" {
			continue // an empty document from a disabled block
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		t.Fatalf("helm template %v rendered nothing", extra)
	}
	return docs
}

// helmTemplate runs the renderer and returns its combined output, so a
// caller testing a deliberate failure can read the message.
func helmTemplate(t *testing.T, extra ...string) ([]byte, error) {
	t.Helper()
	requireHelm(t)
	args := append([]string{"template", "mast", chartDir, "--namespace", releaseNS}, extra...)
	cmd := exec.Command("helm", args...) // #nosec G204 -- test-local, arguments are literals from this file
	return cmd.CombinedOutput()
}

// requireHelm fails — not skips — when the renderer is missing.
//
// The chart is the thing mast ships to operators, and these are the
// only tests over what it produces. A skip here would turn every
// assertion below into a green check on a machine that never ran them,
// which is the failure mode dev/ci/presubmits exists to prevent. The
// opt-out is explicit and greppable.
func requireHelm(t *testing.T) {
	t.Helper()
	if os.Getenv(skipEnv) != "" {
		t.Skipf("%s is set — the chart's RBAC and projection assertions did NOT run", skipEnv)
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm is not on PATH and these tests render the chart to check it.\n"+
			"Install it (https://helm.sh/docs/intro/install/), or set %s=1 to run the rest of the\n"+
			"suite without the chart's RBAC and projection assertions. CI never sets it.", skipEnv)
	}
}

// defaultRender is the chart as an operator gets it: a project ID and
// nothing else. Notably, no remediation namespaces.
func defaultRender(t *testing.T) []doc {
	t.Helper()
	return render(t, "--set", "gcp.projectID="+projectID)
}

// remediatingRender names two namespaces mast may change, which is the
// configuration where the write half of the split exists at all.
func remediatingRender(t *testing.T) []doc {
	t.Helper()
	return render(t,
		"--set", "gcp.projectID="+projectID,
		"--set", "remediationNamespaces={team-a,team-b}")
}

// byName indexes the rendered objects of one kind. The key is
// namespace/name for namespaced kinds, because the chart renders one
// Role per remediation namespace and they deliberately share a name.
func byName(docs []doc, kind string) map[string]doc {
	out := map[string]doc{}
	for _, d := range docs {
		if d.Kind != kind {
			continue
		}
		out[qualified(d.Metadata.Namespace, d.Metadata.Name)] = d
	}
	return out
}

func qualified(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

// treeOf reads every file under dir, keyed by slash-separated path
// relative to dir.
func treeOf(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path) // #nosec G304 -- test-local, repo-relative path
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test-local, repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// shellAssignment returns the quoted value assigned after prefix, e.g.
// `namespaced) CONTAINER_ROLE="roles/container.viewer"` -> the role.
func shellAssignment(script, prefix string) (string, bool) {
	_, rest, ok := strings.Cut(script, prefix)
	if !ok {
		return "", false
	}
	rest = strings.TrimPrefix(rest, `"`)
	value, _, ok := strings.Cut(rest, `"`)
	return value, ok
}
