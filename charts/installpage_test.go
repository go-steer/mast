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

// The chart is what an operator installs and the docs are the only
// instruction they follow, and until now nothing read one against the
// other. #342's "done when" asks for exactly that: the page that tells
// an operator how to install mast is "checked against the release by CI
// rather than by memory". dev/release/check-install-page.sh does it for
// the release tarballs. These do it for the chart, which since #443 is
// the *primary* install instruction and was the one claim on the page
// with no check behind it at all.
//
// What makes the chart half worth its own tests is that its failure is
// silent. `helm --set` accepts a key the chart has never heard of,
// without complaint, and applies it to nothing. So renaming a key in
// values.yaml does not break the documented command — it turns it into
// a no-op:
//
//   gcp.projectID           surfaces immediately. The chart refuses to
//                           render without it (#290, and
//                           TestChartRefusesToRenderWithoutAProject),
//                           so a stale name fails at install time with
//                           a message naming the real key.
//
//   remediationNamespaces   does not surface at all. The install
//                           succeeds and what the operator gets is the
//                           default install this same page warns about
//                           — mast holding no write verb anywhere — on
//                           a cluster where they believe they just
//                           granted two namespaces. A boundary that
//                           reads as if it exists, which is the exact
//                           failure #290 was filed about, arriving
//                           through the documentation instead of
//                           through a template.
//
// The image and chart *references* get the same treatment for the
// reason #440 and #441 exist: the container image deploy/ had always
// referenced was never built and never published, and nothing noticed
// for eight releases, because no test compared a name the docs use
// against a name the release actually pushes.
//
// (The package comment is in render_test.go.)

package charts

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	repoRoot        = ".."
	valuesFile      = chartDir + "/values.yaml"
	installPage     = "../docs/site/src/content/docs/install.md"
	releaseImagesWF = "../.github/workflows/release-images.yml"
	goModFile       = "../go.mod"
)

// Vacuity guards. Every scan below is over files found by walking, and
// a walk that finds nothing passes every assertion in this file. These
// are floors on what the repo contains today, deliberately a little
// below the real counts so that deleting a page is the thing that trips
// them and adding one is not.
const (
	minScannedFiles = 30
	minSetFlags     = 8
	minOCIRefs      = 5
)

// setFlag matches the key of a `--set` argument as it appears in prose.
// The optional leading quote is the shell quoting a list value needs
// (`--set 'remediationNamespaces={a,b}'`), and the key class allows
// helm's escaped-dot form for a map key that contains dots
// (`daemon.nodeSelector."iam\.gke\.io/gke-metadata-server-enabled"`).
//
// `--set-string` and `--set-file` do not match, because `-` is not
// whitespace; if either ever appears in the docs this check will not
// see it, which is worth knowing before assuming the scan is total.
var setFlag = regexp.MustCompile(`--set\s+'?([A-Za-z][A-Za-z0-9_.\-/\\"]*?)=`)

// ociRef matches an OCI chart reference. In mast's docs `oci://` is
// only ever the chart: the sidecar image (ghcr.io/go-steer/k8s-event-watcher)
// and mast's own image are written as plain registry references. That
// is why this check needs no allowlist of "references that are allowed
// to be something else" — a second `oci://` in the docs would be a
// second chart, and failing on it is the right default.
var ociRef = regexp.MustCompile(`oci://([A-Za-z0-9_.\-/]+)`)

// TestDocumentedSetKeysExistInTheChart is the silent-failure check.
//
// Every `--set` the repo tells an operator to run has to name something
// values.yaml actually declares. A key that resolves nowhere is not an
// error at install time, which is the whole problem.
func TestDocumentedSetKeysExistInTheChart(t *testing.T) {
	values := chartValues(t)
	files := operatorFacingFiles(t)

	var seen int
	for _, path := range slices.Sorted(maps.Keys(files)) {
		for _, m := range setFlag.FindAllStringSubmatch(files[path], -1) {
			seen++
			key := m[1]
			if why, ok := resolveHelmPath(values, key); !ok {
				t.Errorf("%s documents `--set %s=…`, and %s.\n"+
					"helm accepts an unknown --set key silently, so this command does not fail — it does nothing.",
					rel(path), key, why)
			}
		}
	}
	if seen < minSetFlags {
		t.Fatalf("found %d --set flags across %d files, expected at least %d — the scan is not reading what it thinks it is",
			seen, len(files), minSetFlags)
	}
}

// TestDocsNameTheChartAndImageTheReleasePublishes compares the names in
// the docs against the names .github/workflows/release-images.yml
// pushes to, in both directions: no documented reference that the
// release does not publish, and no published artifact the docs never
// mention.
func TestDocsNameTheChartAndImageTheReleasePublishes(t *testing.T) {
	image, chart := publishedRefs(t)
	files := operatorFacingFiles(t)

	var refs int
	for _, path := range slices.Sorted(maps.Keys(files)) {
		for _, m := range ociRef.FindAllStringSubmatch(files[path], -1) {
			refs++
			// A reference may carry a :tag or @digest; the repository
			// is what has to match.
			if got := repoOf(m[1]); got != chart {
				t.Errorf("%s points helm at oci://%s, but release-images.yml pushes the chart to %s",
					rel(path), m[1], chart)
			}
		}
	}
	if refs < minOCIRefs {
		t.Fatalf("found %d oci:// references across %d files, expected at least %d", refs, len(files), minOCIRefs)
	}

	// The install page is the one that has to carry them, not merely
	// some page in the corpus.
	page := readFile(t, installPage)
	if !strings.Contains(page, chart) {
		t.Errorf("the install page never names %s, which is where the release publishes the chart", chart)
	}

	// And the chart's own default image has to be the image the release
	// builds. This is the #440/#441 shape: the deployment referenced an
	// image nothing published, and the only thing that would have
	// caught it is comparing these two strings.
	got, _ := values(t, "image", "repository").(string)
	if got != image {
		t.Errorf("charts/mast/values.yaml defaults image.repository to %q, but release-images.yml publishes %q",
			got, image)
	}
}

// TestTheDocumentedRemediationInstallGrantsTheWrite renders the chart
// with the flags copied out of the install page, rather than with
// flags this test file chose.
//
// TestDaemonWriteGrantIsNamespaced already proves the mechanism works
// when it is driven correctly. What it cannot prove is that the command
// on the page drives it correctly — the list syntax `{a,b}` and the
// shell quoting around it are part of the instruction, and a reader
// copy-pastes both. This renders what they would run and checks they
// get the namespaces they asked for.
func TestTheDocumentedRemediationInstallGrantsTheWrite(t *testing.T) {
	if !workloadDeclaresAWrite(t) {
		t.Skip("the deployed workload declares no mutating tool, so no write grant is required")
	}

	namespaces := documentedRemediationNamespaces(t)
	docs := render(t,
		"--set", "gcp.projectID="+projectID,
		"--set", "remediationNamespaces={"+strings.Join(namespaces, ",")+"}")

	want := map[string]bool{}
	for _, ns := range namespaces {
		want[ns] = false
	}
	for _, b := range docs {
		if b.Kind != "RoleBinding" || !bindsDaemon(b) {
			continue
		}
		if _, ok := want[b.Metadata.Namespace]; ok {
			want[b.Metadata.Namespace] = true
		}
	}
	for ns, bound := range want {
		if !bound {
			t.Errorf("the install page's remediation command names %q, and the chart renders no RoleBinding for the daemon there —\n"+
				"the documented command installs mast with no write verb in a namespace the reader believes they granted", ns)
		}
	}
}

// documentedRemediationNamespaces reads the namespace list out of the
// install page's own upgrade command.
func documentedRemediationNamespaces(t *testing.T) []string {
	t.Helper()
	re := regexp.MustCompile(`--set\s+'?remediationNamespaces=\{([^}]*)\}`)
	m := re.FindStringSubmatch(readFile(t, installPage))
	if m == nil {
		t.Fatalf("the install page no longer shows a `--set remediationNamespaces={…}` command.\n" +
			"If the way an operator grants writes changed, this test has to change with it; if the page simply\n" +
			"stopped saying how, that is the regression.")
	}
	var out []string
	for _, ns := range strings.Split(m[1], ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out = append(out, ns)
		}
	}
	if len(out) == 0 {
		t.Fatalf("the install page's remediation command names no namespace")
	}
	return out
}

// publishedRefs reads the image and chart repositories out of the
// release workflow, with the owner expression resolved.
func publishedRefs(t *testing.T) (image, chart string) {
	t.Helper()
	var wf struct {
		Env map[string]string `yaml:"env"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, releaseImagesWF)), &wf); err != nil {
		t.Fatalf("parse %s: %v", releaseImagesWF, err)
	}
	owner := repoOwner(t)
	resolve := func(key string) string {
		v, ok := wf.Env[key]
		if !ok {
			t.Fatalf("%s no longer defines a top-level env.%s — this test reads the published names from there",
				releaseImagesWF, key)
		}
		// The workflow writes the owner as an expression so a fork
		// publishes under its own account. The docs cannot; they name
		// go-steer. Substituting here is what lets the two be compared
		// at all, and it is the repository's own owner, taken from
		// go.mod rather than written down a second time.
		return strings.NewReplacer(
			"${{ github.repository_owner }}", owner,
			"${{github.repository_owner}}", owner,
		).Replace(v)
	}
	image, chart = resolve("IMAGE"), resolve("CHART_REPO")
	if strings.Contains(image+chart, "${{") {
		t.Fatalf("could not resolve the published names: image=%q chart=%q", image, chart)
	}
	return image, chart
}

// repoOwner is the org segment of the module path, so the owner is
// stated once in the repository rather than twice.
func repoOwner(t *testing.T) string {
	t.Helper()
	for _, line := range strings.Split(readFile(t, goModFile), "\n") {
		path, ok := strings.CutPrefix(strings.TrimSpace(line), "module ")
		if !ok {
			continue
		}
		parts := strings.Split(strings.TrimSpace(path), "/")
		if len(parts) < 3 {
			t.Fatalf("module path %q has no owner segment", path)
		}
		return parts[1]
	}
	t.Fatalf("no module line in %s", goModFile)
	return ""
}

// repoOf strips a :tag or @digest from a registry reference. The colon
// search starts after the last slash so a registry port is not mistaken
// for a tag.
func repoOf(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	last := strings.LastIndex(ref, "/")
	if i := strings.Index(ref[last+1:], ":"); i >= 0 {
		ref = ref[:last+1+i]
	}
	return ref
}

// chartValues decodes values.yaml once for the whole file.
func chartValues(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, valuesFile)), &v); err != nil {
		t.Fatalf("parse %s: %v", valuesFile, err)
	}
	if len(v) == 0 {
		t.Fatalf("%s decoded to nothing", valuesFile)
	}
	return v
}

// values reads a path out of values.yaml, for the single-lookup cases.
func values(t *testing.T, path ...string) any {
	t.Helper()
	var cur any = chartValues(t)
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	return cur
}

// resolveHelmPath reports whether a `--set` key names something the
// chart declares, and if not, why not in the words the failure message
// needs.
//
// An empty map terminates the walk successfully. `daemon.nodeSelector:
// {}` and `commonLabels: {}` are open by design — arbitrary keys under
// them are the point — so requiring the leaf to exist would fail the
// chart's own documented nodeSelector example.
func resolveHelmPath(values map[string]any, key string) (string, bool) {
	var cur any = values
	walked := ""
	for i, seg := range splitHelmPath(key) {
		m, ok := cur.(map[string]any)
		if !ok {
			return "values.yaml has " + walked + ", which is not a map, so there is nothing under it to set", false
		}
		if len(m) == 0 {
			return "", true // an open map: any key under it is legitimate
		}
		if _, ok := m[seg]; !ok {
			where := "at the top level of values.yaml"
			if i > 0 {
				where = "under " + walked
			}
			return "values.yaml declares no " + seg + " " + where, false
		}
		if walked == "" {
			walked = seg
		} else {
			walked += "." + seg
		}
		cur = m[seg]
	}
	return "", true
}

// splitHelmPath splits a --set key the way helm does: on dots that are
// neither backslash-escaped nor inside double quotes.
func splitHelmPath(key string) []string {
	var out []string
	var cur strings.Builder
	var quoted bool
	for i := 0; i < len(key); i++ {
		switch c := key[i]; {
		case c == '\\' && i+1 < len(key):
			i++
			cur.WriteByte(key[i])
		case c == '"':
			quoted = !quoted
		case c == '.' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

// operatorFacingFiles is every file in the repository that can tell an
// operator to run something: all markdown, plus the chart itself, whose
// values.yaml comments, NOTES.txt and required-value message all print
// `--set` commands at people.
//
// charts/mast/files is excluded: it is the workload bundle — specialist
// prompts and JSON schemas — not instructions to an operator, and a
// prompt that happens to contain the characters `--set` is not a claim
// about this chart.
func operatorFacingFiles(t *testing.T) map[string]string {
	t.Helper()
	skipDir := []string{
		filepath.Join(repoRoot, "docs", "site", "dist"),
		filepath.Join(repoRoot, "docs", "site", "node_modules"),
		filepath.Join(repoRoot, chartDir, "files"),
	}

	out := map[string]string{}
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// path == repoRoot is "..", whose Name() starts with a dot
			// and would otherwise skip the entire walk on its first
			// call. The vacuity guard below is what caught that.
			if name := d.Name(); path != repoRoot && strings.HasPrefix(name, ".") && name != ".github" {
				return filepath.SkipDir
			}
			if slices.Contains(skipDir, filepath.Clean(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		inChart := strings.HasPrefix(filepath.ToSlash(filepath.Clean(path)), chartDir+"/")
		if filepath.Ext(path) != ".md" && !inChart {
			return nil
		}
		data, err := os.ReadFile(path) // #nosec G304 -- test-local, repo-relative path
		if err != nil {
			return err
		}
		out[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
	if len(out) < minScannedFiles {
		t.Fatalf("scanned %d files, expected at least %d — the walk is missing the docs tree", len(out), minScannedFiles)
	}
	return out
}

// rel names a file the way the repository does, so a failure message
// can be pasted into an editor.
func rel(path string) string {
	if p, err := filepath.Rel(repoRoot, path); err == nil {
		return filepath.ToSlash(p)
	}
	return path
}
