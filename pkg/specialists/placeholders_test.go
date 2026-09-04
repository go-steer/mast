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

package specialists

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The rule these tests describe is ADK's, not mast's: mast only decides
// when to say so. TestThePlaceholderRuleIsStillADKs, at the bottom,
// checks the copy against the source it was copied from.

func TestATemplateThatLooksUpSessionStateIsRefused(t *testing.T) {
	err := checkPlaceholders("Diagnose.tmpl", "Investigate the workload in {project} and report.")
	if err == nil {
		t.Fatal("a template asking for state key \"project\" loaded; it would have died on its first run")
	}
	for _, want := range []string{"Diagnose.tmpl", "line 1", "{project}", `"project"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s:\n%v", want, err)
		}
	}
}

func TestBracesAroundAnythingElseAreLiteral(t *testing.T) {
	// Everything ADK hands back verbatim. A prompt full of manifests,
	// jsonpath and prose is the normal case and must keep loading.
	for _, body := range []string{
		`Patch it with {"spec":{"replicas":1}}.`,
		`Read the taint off {.spec.template.spec.nodeSelector}.`,
		"The node is {context.node}, which the caller fills in.",
		"Omit the middle with {...} when you quote a log line.",
		"A selector is written {app: web}, with the space.",
		"An empty pair {} is nothing at all.",
		"Nothing in braces here.",
	} {
		if err := checkPlaceholders("Diagnose.tmpl", body); err != nil {
			t.Errorf("refused a literal body %q:\n%v", body, err)
		}
	}
}

func TestTheSpaceAfterTheColonDecidesIt(t *testing.T) {
	// The asymmetry worth a test of its own: "app:" is one of ADK's
	// three state scopes, so the spaced form is prose and the unspaced
	// form is a lookup. Nothing about the two shapes says so.
	if err := checkPlaceholders("Diagnose.tmpl", "label it {app: web}"); err != nil {
		t.Errorf("{app: web} is not a valid state name and must load:\n%v", err)
	}
	err := checkPlaceholders("Diagnose.tmpl", "label it {app:web}")
	if err == nil {
		t.Fatal("{app:web} is a lookup of the app-scoped key \"web\" and must be refused")
	}
	if !strings.Contains(err.Error(), `"app:web"`) {
		t.Errorf("the error does not name the scoped key it will look up:\n%v", err)
	}
}

func TestTheOptionalMarkerIsTheWayToAskForState(t *testing.T) {
	for _, body := range []string{
		"Investigate {project?} if the caller named one.",
		"Investigate {app:project?} if the caller named one.",
	} {
		if err := checkPlaceholders("Diagnose.tmpl", body); err != nil {
			t.Errorf("refused %q, which cannot fail a run:\n%v", body, err)
		}
	}
}

func TestDoublingTheBracesDoesNotEscapeIt(t *testing.T) {
	// The first thing anyone tries. The regex matches runs of braces
	// and the trim takes all of them, so it is the same lookup.
	err := checkPlaceholders("Diagnose.tmpl", "Investigate {{project}}.")
	if err == nil {
		t.Fatal("{{project}} loaded; ADK trims it to the same key as {project}")
	}
	if !strings.Contains(err.Error(), "{{x}}") {
		t.Errorf("the error should say doubling is not an escape, since that is what the author just tried:\n%v", err)
	}
}

func TestAnArtifactPlaceholderIsRefusedEvenWhenOptional(t *testing.T) {
	// ADK demands an artifact service before it consults the optional
	// marker, and mast runs none, so the marker rescues nothing here.
	for _, body := range []string{
		"Quote the summary from {artifact.report}.",
		"Quote the summary from {artifact.report?}.",
	} {
		err := checkPlaceholders("Diagnose.tmpl", body)
		if err == nil {
			t.Fatalf("%q loaded; there is no artifact service for it to load from", body)
		}
		if !strings.Contains(err.Error(), "artifact") || !strings.Contains(err.Error(), `"report"`) {
			t.Errorf("the error does not name the artifact:\n%v", err)
		}
	}
	if err := checkPlaceholders("Diagnose.tmpl", "Quote {artifact.report?}."); !strings.Contains(err.Error(), "no artifact service") {
		t.Errorf("the error should say why the marker did not help:\n%v", err)
	}
}

func TestEveryOffenderIsNamedWithItsLine(t *testing.T) {
	// All of them, because an author who fixes one and restarts to find
	// the next learns the rule the slowest possible way.
	body := "Investigate {project}.\nIt runs in {region}.\nOwned by {temp:team}.\n"
	err := checkPlaceholders("Diagnose.tmpl", body)
	if err == nil {
		t.Fatal("three lookups loaded")
	}
	for _, want := range []string{"line 1", "line 2", "line 3", "{project}", "{region}", "{temp:team}", "3 placeholders"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s:\n%v", want, err)
		}
	}
}

func TestTheShippedTemplatesStillLoad(t *testing.T) {
	// The catalog is full of manifests and jsonpath. If the check is
	// wrong about what is literal, this is where it shows.
	dirs := []string{
		filepath.Join("..", "..", "examples", "workloads", "gke-triage", "specialists"),
		filepath.Join("..", "..", "deploy", "base", "config", "specialists"),
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Skipf("no %s to check", dir)
		}
		if _, err := LoadDir(dir); err != nil {
			t.Errorf("the shipped catalog in %s no longer loads:\n%v", dir, err)
		}
	}
}

func TestThePlaceholderRuleIsStillADKs(t *testing.T) {
	// placeholders.go copies four things out of
	// google.golang.org/adk/v2/internal/llminternal, which no package
	// outside ADK can import. Pin the copy to the source so an ADK
	// bump that changes the rule fails here, rather than as a template
	// mast accepted and ADK died on.
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "google.golang.org/adk/v2").Output()
	if err != nil {
		t.Skipf("cannot locate the adk module: %v", err)
	}
	path := filepath.Join(strings.TrimSpace(string(out)), "internal", "llminternal", "instruction_processor.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}
	adk := string(src)

	for what, want := range map[string]string{
		"the placeholder regex": "regexp.MustCompile(`{+[^{}]*}+`)",
		"the trim":              `strings.TrimSpace(strings.Trim(match, "{}"))`,
		"the optional marker":   `strings.HasSuffix(varName, "?")`,
		"the artifact prefix":   `strings.CutPrefix(varName, "artifact.")`,
		"the app scope":         `appPrefix  = "app:"`,
		"the user scope":        `userPrefix = "user:"`,
		"the temp scope":        `tempPrefix = "temp:"`,
	} {
		if !strings.Contains(adk, want) {
			t.Errorf("%s changed in ADK: %s is no longer in %s — re-read replaceMatch and update placeholders.go", what, want, path)
		}
	}

	// The ordering matters as much as the pieces: mast flags an
	// optional artifact because ADK demands a service before it looks
	// at the marker. If that inverts, {artifact.x?} becomes safe.
	service := strings.Index(adk, "artifact service is not initialized")
	optional := strings.Index(adk, "failed to load artifact")
	if service < 0 || optional < 0 || service > optional {
		t.Error("ADK no longer requires an artifact service before consulting the optional marker; " +
			"an optional artifact placeholder may now be safe, and placeholders.go should stop refusing it")
	}
}
