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
	"path/filepath"
	"strings"
	"testing"
)

// These tests describe a migration, not a rule. mast sends instructions
// verbatim (pkg/agent/instruction.go), so the only thing left to say
// about a brace is that one spelling of it used to mean something and
// stopped. Everything else loads, which is most of what this file
// checks.
//
// The ADK-parity test that used to live at the bottom is gone with the
// coupling it guarded: mast no longer follows ADK's resolution rule, so
// an ADK bump that changed the rule would have failed a test demanding
// mast track a field it does not use. What must not regress is the
// cause, and pkg/agent's
// TestNoShippedCodePassesAPromptThroughADKsTemplateField guards that
// across the whole module.

func TestTheOptionalMarkerIsRefusedBecauseItStoppedInjecting(t *testing.T) {
	// The one syntax whose meaning changed silently. An author wrote it
	// to ask for session state; left alone it would render as itself.
	for _, tc := range []struct{ body, key string }{
		{"Investigate {project?} if the caller named one.", `"project"`},
		{"Investigate {app:project?} if the caller named one.", `"app:project"`},
	} {
		err := checkPlaceholders("Diagnose.specialist.md", tc.body)
		if err == nil {
			t.Fatalf("%q loaded; its marker no longer injects and the author is not told", tc.body)
		}
		for _, want := range []string{"Diagnose.specialist.md", "line 1", tc.key, "verbatim"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not mention %s:\n%v", want, err)
			}
		}
	}
}

func TestEverythingThatUsedToBeRefusedNowLoads(t *testing.T) {
	// The substance of the change. Each of these was refused at load
	// while mast used ADK's template field, and each is ordinary text
	// now. A prompt is not a template; refusing prose would be mast
	// restricting what it has promised to pass through.
	for _, body := range []string{
		"Investigate the workload in {project} and report.",
		"Label it {app:web} before you patch.",
		"Quote the summary from {artifact.report}.",
		"Investigate {{project}}.",
		"Owned by {temp:team}.",
		`Run "${MAST_HOME}/bin/mast" -p check before you start.`,
		`Emit {"severity": "high", "finding": "..."} and nothing else.`,
	} {
		if err := checkPlaceholders("Diagnose.specialist.md", body); err != nil {
			t.Errorf("refused %q, which is literal text the model should simply read:\n%v", body, err)
		}
	}
}

func TestBracesAroundAnythingElseAreStillLiteral(t *testing.T) {
	// Unchanged by this file's narrowing, and kept because it is the
	// normal case: a catalog full of manifests, jsonpath and prose.
	for _, body := range []string{
		`Patch it with {"spec":{"replicas":1}}.`,
		`Read the taint off {.spec.template.spec.nodeSelector}.`,
		"The node is {context.node}, which the caller fills in.",
		"Omit the middle with {...} when you quote a log line.",
		"A selector is written {app: web}, with the space.",
		"An empty pair {} is nothing at all.",
		"Nothing in braces here.",
	} {
		if err := checkPlaceholders("Diagnose.specialist.md", body); err != nil {
			t.Errorf("refused a literal body %q:\n%v", body, err)
		}
	}
}

func TestAQuestionMarkInProseIsNotAMarker(t *testing.T) {
	// The narrowing keys off a trailing "?", so the thing to get wrong
	// is refusing an author who ended a braced aside with one. What
	// preceded the marker still has to be a name ADK would have looked
	// up.
	for _, body := range []string{
		"Ask the operator {who knows?} before acting.",
		"A bare {?} is nothing at all.",
		"A selector is written {app: web?}, with the space.",
	} {
		if err := checkPlaceholders("Diagnose.specialist.md", body); err != nil {
			t.Errorf("refused %q, which never resolved to anything:\n%v", body, err)
		}
	}
}

func TestAnOptionalArtifactIsRefusedAndHasNoReplacement(t *testing.T) {
	// Refused for the same migration reason as the state form, but the
	// advice differs: this one never loaded anything, so there is
	// nothing for the author to port it to.
	err := checkPlaceholders("Diagnose.specialist.md", "Quote the summary from {artifact.report?}.")
	if err == nil {
		t.Fatal("{artifact.report?} loaded; its marker no longer means anything")
	}
	for _, want := range []string{`"report"`, "no artifact service", "nothing to replace it with"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s:\n%v", want, err)
		}
	}
}

func TestEveryOffenderIsNamedWithItsLine(t *testing.T) {
	// All of them, because an author who fixes one and restarts to find
	// the next learns the rule the slowest possible way.
	body := "Investigate {project?}.\nIt runs in {region?}.\nOwned by {temp:team?}.\n"
	err := checkPlaceholders("Diagnose.specialist.md", body)
	if err == nil {
		t.Fatal("three stale markers loaded")
	}
	for _, want := range []string{"line 1", "line 2", "line 3", "{project?}", "{region?}", "{temp:team?}", "3 placeholders"} {
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
