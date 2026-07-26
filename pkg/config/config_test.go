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

package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempDir returns a fresh directory under os.TempDir() (house rule:
// scratch state never goes under $HOME). t.TempDir honors TMPDIR and
// defaults to os.TempDir, and cleans up automatically.
func tempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

// writeSpecialist writes a minimal valid .tmpl. name == "" omits the
// explicit name (filename-derived).
func writeSpecialist(t *testing.T, dir, filename, name string) {
	t.Helper()
	nameLine := ""
	if name != "" {
		nameLine = "name: " + name + "\n"
	}
	body := fmt.Sprintf("---\n%sdescription: Test specialist.\nmode: Task\n---\nDo the thing.\n", nameLine)
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(body), 0o644); err != nil {
		t.Fatalf("write specialist: %v", err)
	}
}

// writeWorkload writes a minimal valid workload bundle referencing the
// given specialist roster.
func writeWorkload(t *testing.T, dir, filename, name string, roster ...string) {
	t.Helper()
	var sb strings.Builder
	fmt.Fprintf(&sb, "name: %s\nbudget:\n  max_wallclock_seconds: 300\n  max_cost_usd: 1.5\nspecialists:\n", name)
	for _, r := range roster {
		fmt.Fprintf(&sb, "  - %s\n", r)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write workload: %v", err)
	}
}

// populateRoot fills root with one workload (wlName) and its one
// specialist (spName).
func populateRoot(t *testing.T, root, wlName, spName string) {
	t.Helper()
	writeSpecialist(t, mkdir(t, filepath.Join(root, "specialists")), spName+".tmpl", "")
	writeWorkload(t, mkdir(t, filepath.Join(root, "workloads")), wlName+".yaml", wlName, spName)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestDiscoverSelectionOrder(t *testing.T) {
	base := tempDir(t)
	envDir := mkdir(t, filepath.Join(base, "envroot"))
	cwd := mkdir(t, filepath.Join(base, "cwd"))
	projDir := filepath.Join(cwd, ".agents")
	userDir := filepath.Join(base, "user", "mast", "agents")
	sysDir := filepath.Join(base, "etc", "mast", "agents")

	// Nothing exists: error naming the consulted locations.
	if _, err := discover("", cwd, userDir, sysDir); err == nil {
		t.Fatal("want error when no location exists")
	}

	// System only.
	mkdir(t, sysDir)
	r, err := discover("", cwd, userDir, sysDir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceSystem || r.Dir != sysDir {
		t.Fatalf("want system root, got %+v", r)
	}

	// User beats system.
	mkdir(t, userDir)
	r, err = discover("", cwd, userDir, sysDir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceUser || r.Dir != userDir {
		t.Fatalf("want user root, got %+v", r)
	}
	if len(r.Shadowed) != 1 || r.Shadowed[0] != sysDir {
		t.Fatalf("want system dir reported shadowed, got %v", r.Shadowed)
	}

	// Project beats user + system.
	mkdir(t, projDir)
	r, err = discover("", cwd, userDir, sysDir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceProject || r.Dir != projDir {
		t.Fatalf("want project root, got %+v", r)
	}
	if len(r.Shadowed) != 2 {
		t.Fatalf("want user+system shadowed, got %v", r.Shadowed)
	}

	// Env beats everything, and reports no shadows (nothing else is
	// even consulted).
	r, err = discover(envDir, cwd, userDir, sysDir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceEnv || r.Dir != envDir {
		t.Fatalf("want env root, got %+v", r)
	}

	// Env set but missing: fatal, no fallthrough.
	if _, err := discover(filepath.Join(base, "nope"), cwd, userDir, sysDir); err == nil {
		t.Fatal("want error for missing MAST_CONFIG_DIR, not fallthrough")
	}
}

func TestDiscoverPublicEnvVar(t *testing.T) {
	root := tempDir(t)
	t.Setenv(EnvConfigDir, root)
	r, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceEnv {
		t.Fatalf("want SourceEnv, got %+v", r)
	}
}

// TestEmptyProjectDirShadowsPopulatedUserDir pins the documented
// footgun: exclusive discovery means an existing-but-empty ./.agents
// wins outright over a populated user dir — no merging, the user dir's
// workloads are simply invisible.
func TestEmptyProjectDirShadowsPopulatedUserDir(t *testing.T) {
	base := tempDir(t)
	cwd := mkdir(t, filepath.Join(base, "cwd"))
	projDir := mkdir(t, filepath.Join(cwd, ".agents")) // exists, empty
	userDir := mkdir(t, filepath.Join(base, "user", "mast", "agents"))
	populateRoot(t, userDir, "incident-triage", "classifier")

	r, err := discover("", cwd, userDir, filepath.Join(base, "no-sys"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Dir != projDir || r.Source != SourceProject {
		t.Fatalf("want empty project dir to win outright, got %+v", r)
	}
	if len(r.Shadowed) != 1 || r.Shadowed[0] != userDir {
		t.Fatalf("want populated user dir reported as shadowed, got %v", r.Shadowed)
	}

	cfg, err := LoadRoot(r, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workloads) != 0 || len(cfg.Specialists) != 0 {
		t.Fatalf("empty selected root must yield nothing (no cross-location merge); got %d workloads, %d specialists",
			len(cfg.Workloads), len(cfg.Specialists))
	}
	if _, ok := cfg.Workloads["incident-triage"]; ok {
		t.Fatal("workload from the shadowed user dir must NOT be visible")
	}
}

func TestLoadRootHappyPath(t *testing.T) {
	root := tempDir(t)
	populateRoot(t, root, "incident-triage", "classifier")
	writeWorkload(t, filepath.Join(root, "workloads"), "drift.yml", "drift-detection", "classifier")
	// Nested subdirectory must be ignored (flat, non-recursive scan).
	archive := mkdir(t, filepath.Join(root, "workloads", "archive"))
	writeWorkload(t, archive, "old.yaml", "retired-workload", "classifier")

	cfg, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workloads) != 2 {
		t.Fatalf("want 2 workloads (.yaml + .yml, no recursion), got %v", sortedKeys(cfg.Workloads))
	}
	if _, ok := cfg.Workloads["retired-workload"]; ok {
		t.Fatal("nested workloads/archive/ must be ignored")
	}
	if _, ok := cfg.Specialists["classifier"]; !ok {
		t.Fatalf("want specialist %q loaded, got %v", "classifier", sortedKeys(cfg.Specialists))
	}
	b := cfg.Workloads["incident-triage"]
	if b.Budget.MaxCostUSD != 1.5 || b.Budget.MaxWallclockSeconds != 300 {
		t.Fatalf("file budget values not preserved: %+v", b.Budget)
	}
}

func TestWorkloadNameCollisionFatal(t *testing.T) {
	root := tempDir(t)
	populateRoot(t, root, "incident-triage", "classifier")
	// Different filename, same declared name.
	writeWorkload(t, filepath.Join(root, "workloads"), "other.yaml", "incident-triage", "classifier")

	_, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger())
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("want fatal name collision, got %v", err)
	}
}

func TestSpecialistNameCollisionFatal(t *testing.T) {
	root := tempDir(t)
	sp := mkdir(t, filepath.Join(root, "specialists"))
	writeSpecialist(t, sp, "a.tmpl", "classifier") // explicit name
	writeSpecialist(t, sp, "classifier.tmpl", "")  // filename-derived

	_, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger())
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("want fatal name collision, got %v", err)
	}
}

func TestRosterReferenceMissingSpecialistFatal(t *testing.T) {
	root := tempDir(t)
	populateRoot(t, root, "incident-triage", "classifier")
	writeWorkload(t, filepath.Join(root, "workloads"), "broken.yaml", "broken", "no-such-specialist")

	_, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger())
	if err == nil || !strings.Contains(err.Error(), "no-such-specialist") {
		t.Fatalf("want fatal missing-roster-reference error, got %v", err)
	}
}

func TestBudgetEnvOverrides(t *testing.T) {
	root := tempDir(t)
	populateRoot(t, root, "incident-triage", "classifier")

	t.Setenv(EnvBudgetMaxCostUSD, "0.02")
	t.Setenv(EnvBudgetMaxWallclockSeconds, "42")

	cfg, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Workloads["incident-triage"]
	if b.Budget.MaxCostUSD != 0.02 {
		t.Fatalf("%s not applied: got %v", EnvBudgetMaxCostUSD, b.Budget.MaxCostUSD)
	}
	if b.Budget.MaxWallclockSeconds != 42 {
		t.Fatalf("%s not applied: got %v", EnvBudgetMaxWallclockSeconds, b.Budget.MaxWallclockSeconds)
	}
}

func TestBudgetEnvOverrideUnparseableIsFatal(t *testing.T) {
	root := tempDir(t)
	populateRoot(t, root, "incident-triage", "classifier")

	t.Setenv(EnvBudgetMaxCostUSD, "not-a-number")
	if _, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger()); err == nil {
		t.Fatal("want fatal error for unparseable env override")
	}
}

func TestBudgetEnvOverrideEmptyStringIsUnset(t *testing.T) {
	root := tempDir(t)
	populateRoot(t, root, "incident-triage", "classifier")

	t.Setenv(EnvBudgetMaxCostUSD, "")
	cfg, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger())
	if err != nil {
		t.Fatalf("empty-string override must be treated as unset, got %v", err)
	}
	if got := cfg.Workloads["incident-triage"].Budget.MaxCostUSD; got != 1.5 {
		t.Fatalf("file value must be preserved when override is empty, got %v", got)
	}
}

// writeA2AAgent writes a minimal valid .agents/a2a registration.
func writeA2AAgent(t *testing.T, dir, filename, name string) {
	t.Helper()
	body := fmt.Sprintf("name: %s\nagent_card_url: https://%s.example.com\nauth:\n  type: bearer\n  token_env: TEST_TOKEN\n", name, name)
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(body), 0o644); err != nil {
		t.Fatalf("write a2a agent: %v", err)
	}
}

func TestLoadRootA2AAgents(t *testing.T) {
	root := tempDir(t)
	populateRoot(t, root, "incident-triage", "classifier")
	a2aDir := mkdir(t, filepath.Join(root, "a2a"))
	writeA2AAgent(t, a2aDir, "external-triage.yaml", "external-triage")
	writeA2AAgent(t, a2aDir, "scanner.yml", "scanner")

	cfg, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.A2A) != 2 {
		t.Fatalf("want 2 a2a agents, got %v", sortedKeys(cfg.A2A))
	}
	if cfg.A2A["external-triage"].Auth.TokenEnv != "TEST_TOKEN" {
		t.Fatalf("a2a agent fields not preserved: %+v", cfg.A2A["external-triage"])
	}
	list := cfg.A2AList()
	if len(list) != 2 || list[0].Name != "external-triage" || list[1].Name != "scanner" {
		t.Fatalf("A2AList() = %+v, want name-sorted registrations", list)
	}
}

func TestLoadRootA2AMissingDirIsEmpty(t *testing.T) {
	root := tempDir(t)
	populateRoot(t, root, "incident-triage", "classifier")
	cfg, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.A2A) != 0 {
		t.Fatalf("want no a2a agents, got %v", sortedKeys(cfg.A2A))
	}
}

func TestA2ANameCollisionFatal(t *testing.T) {
	root := tempDir(t)
	populateRoot(t, root, "incident-triage", "classifier")
	a2aDir := mkdir(t, filepath.Join(root, "a2a"))
	writeA2AAgent(t, a2aDir, "a.yaml", "external-triage")
	writeA2AAgent(t, a2aDir, "b.yaml", "external-triage")

	_, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger())
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("want fatal a2a name collision, got %v", err)
	}
}

func TestA2AInvalidConfigFatal(t *testing.T) {
	root := tempDir(t)
	populateRoot(t, root, "incident-triage", "classifier")
	a2aDir := mkdir(t, filepath.Join(root, "a2a"))
	if err := os.WriteFile(filepath.Join(a2aDir, "bad.yaml"), []byte("name: bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRoot(Root{Dir: root, Source: SourceProject}, testLogger())
	if err == nil || !strings.Contains(err.Error(), "agent_card_url or endpoint") {
		t.Fatalf("want fatal a2a validation error, got %v", err)
	}
}
