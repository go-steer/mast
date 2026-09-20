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

package workload

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// minimal is the smallest bundle that passes validate(), so a case can
// add exactly the one line it is about and nothing else.
const minimal = `name: probe
specialists:
  - analyst
`

func loadYAML(t *testing.T, body string) (Bundle, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workload.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// TestAbsentSchemaVersionMeansOne is the compatibility promise. Every
// bundle in this tree, in the private siblings, and in whatever an
// operator has checked into their own repo predates the field; not one
// of them may change behaviour because it was added.
func TestAbsentSchemaVersionMeansOne(t *testing.T) {
	b, err := loadYAML(t, minimal)
	if err != nil {
		t.Fatalf("a bundle with no schema_version must load: %v", err)
	}
	if b.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1 — absent resolves to 1 so downstream never sees a zero", b.SchemaVersion)
	}
}

// TestSchemaVersionOneLoads pins the explicit spelling too, so that
// writing the field down is never worse than omitting it.
func TestSchemaVersionOneLoads(t *testing.T) {
	b, err := loadYAML(t, "schema_version: 1\n"+minimal)
	if err != nil {
		t.Fatalf("explicit schema_version: 1 must load: %v", err)
	}
	if b.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", b.SchemaVersion)
	}
}

// TestBundleFromTheFutureIsRefused is the whole point of the field. The
// error has to name both numbers: an operator holding a bundle and a
// binary needs to know which one to change.
func TestBundleFromTheFutureIsRefused(t *testing.T) {
	future := strconv.Itoa(CurrentSchemaVersion + 1)
	_, err := loadYAML(t, "schema_version: "+future+"\n"+minimal)
	if err == nil {
		t.Fatal("a bundle declaring a newer schema loaded; it must be refused")
	}
	for _, want := range []string{future, strconv.Itoa(CurrentSchemaVersion)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

// TestTheVersionErrorWinsOverTheUnknownKeyError is the ordering the
// two-pass decode exists for, and it is the case most likely to regress
// if someone collapses Load back into a single strict Unmarshal.
//
// A real bundle from the future does not arrive carrying only a version
// bump — it arrives carrying the keys that motivated the bump. Under a
// single strict pass the operator is told "field quarantine_policy not
// found", which reads as a typo in a key they copied out of newer
// documentation, and sends them looking in the wrong place. The version
// mismatch is the true cause and has to be the reported one.
func TestTheVersionErrorWinsOverTheUnknownKeyError(t *testing.T) {
	future := strconv.Itoa(CurrentSchemaVersion + 1)
	_, err := loadYAML(t, "schema_version: "+future+"\nquarantine_policy: strict\n"+minimal)
	if err == nil {
		t.Fatal("a future bundle with future keys loaded")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("refusal blames something other than the version: %v", err)
	}
	if strings.Contains(err.Error(), "quarantine_policy") {
		t.Errorf("refusal blames the unknown key, which is the symptom not the cause: %v", err)
	}
}

// TestSchemaVersionZeroIsRefused separates "did not say" from "said
// something wrong". Absent is the common case and means 1; an explicit 0
// is someone reaching for a version number, and reading it as 1 would
// hide the mistake.
func TestSchemaVersionZeroIsRefused(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		if _, err := loadYAML(t, "schema_version: "+v+"\n"+minimal); err == nil {
			t.Errorf("schema_version: %s loaded; it must be refused", v)
		}
	}
}

// TestUnknownTopLevelKeyIsRefused is the decision #302 asked to be made
// deliberately: an unrecognised key is a load error, not a warning.
//
// The argument is the file's job. A bundle is the only place a tool can
// be declared safe to run without an operator, and mast's predicate is
// default-deny-unknown, so the failure being designed against is a
// misspelled block that leaves a whole section of policy unapplied while
// the daemon reports a clean start. A WARN in a log nobody reads at
// boot is not a control.
func TestUnknownTopLevelKeyIsRefused(t *testing.T) {
	_, err := loadYAML(t, "tool_catalogue:\n  tools: []\n"+minimal)
	if err == nil {
		t.Fatal("a misspelled top-level key loaded; it must be refused")
	}
	if !strings.Contains(err.Error(), "tool_catalogue") {
		t.Errorf("refusal does not name the key: %v", err)
	}
}

// TestUnknownNestedKeyIsRefused checks the strictness reaches all the
// way down. `mutating` sitting one level out is the exact shape of the
// bug worth refusing: the tool is still catalogued, so nothing looks
// missing, and it silently reverts to default-deny-unknown.
func TestUnknownNestedKeyIsRefused(t *testing.T) {
	_, err := loadYAML(t, `name: probe
specialists:
  - analyst
tool_catalog:
  tools:
    - name: patch_resource
      mutatin: true
`)
	if err == nil {
		t.Fatal("a misspelled nested key loaded; it must be refused")
	}
	if !strings.Contains(err.Error(), "mutatin") {
		t.Errorf("refusal does not name the key: %v", err)
	}
}

// TestShippedBundlesStillLoad is the blast-radius guard for the
// strictness. Every bundle this repo ships is a bundle an operator may
// have copied; if one of them stops loading, the change broke real
// files and the test says which.
func TestShippedBundlesStillLoad(t *testing.T) {
	roots := []string{
		"../../charts/mast/files/workload.yaml",
		"../../examples/workloads/gke-triage/workload.yaml",
		"../../examples/workloads/ns-audit/workload.yaml",
		"../../examples/workloads/bounded-triage/workload.yaml",
		"../../testdata/uat/workload.yaml",
		"../../testdata/live/workload.yaml",
	}
	for _, p := range roots {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v — if a shipped bundle moved, update this list rather than dropping the check", p, err)
			continue
		}
		if _, err := Load(p); err != nil {
			t.Errorf("shipped bundle %s no longer loads: %v", p, err)
		}
	}
}
