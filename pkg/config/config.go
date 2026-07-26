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

// Package config implements v0.1 of the `.agents/` discovery and
// loading rules from docs/config-layout-design.md.
//
// # Discovery
//
// The `.agents/` root is looked up in exactly one location per
// invocation — EXCLUSIVE, first match wins, no cross-location merging:
//
//  1. $MAST_CONFIG_DIR — if set, it is the canonical location and
//     nothing else is consulted. If it does not exist, that is a fatal
//     error (we never silently fall through past an explicit override).
//  2. ./.agents in the process working directory.
//  3. <user config dir>/mast/agents (os.UserConfigDir; XDG-compliant
//     on Linux, i.e. ~/.config/mast/agents).
//  4. /etc/mast/agents (system-level).
//
// Because selection is exclusive, an EXISTING-but-EMPTY higher-priority
// location shadows a populated lower-priority one. That is by design
// (deterministic, no merge-order bugs) but is a known operator footgun,
// so loading logs loudly: which root was selected, why, what was found
// in it, and which existing lower-priority locations it shadows.
//
// # File discovery within the root
//
// Per-directory scans are flat and non-recursive; nested subdirectories
// are ignored (operators may keep e.g. workloads/archive/):
//
//   - <root>/workloads/*.yaml (also *.yml) — parsed by pkg/workload.
//   - <root>/specialists/*.tmpl — parsed by pkg/specialists.
//
// A missing subdirectory yields zero entries; it is not an error.
// Two files defining the same name in the same directory are a fatal
// load-time error (v0.1: fail fast, operator resolves). All load-time
// validation errors are fatal — mast refuses to start on invalid
// config. There is no hot-reload in v0.1; changes require restart.
//
// # Env-var overrides
//
// Scalar config values can be overridden by environment variables
// following the convention from config-layout-design.md: the config
// key path, uppercased, dots/nesting replaced with underscores, with
// the MAST_ prefix. For the workload budget block implemented in v0.1:
//
//	budget.max_cost_usd          → MAST_BUDGET_MAX_COST_USD
//	budget.max_wallclock_seconds → MAST_BUDGET_MAX_WALLCLOCK_SECONDS
//
// Env overrides are process-wide: they apply to every workload bundle
// loaded in this invocation, and they override file values
// unconditionally. A set-but-unparseable override is a fatal load-time
// error.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvConfigDir is the env var that, when set, exclusively selects the
// `.agents/` root.
const EnvConfigDir = "MAST_CONFIG_DIR"

// Source identifies which discovery rule selected the `.agents/` root.
type Source string

const (
	// SourceEnv — $MAST_CONFIG_DIR was set.
	SourceEnv Source = "env:MAST_CONFIG_DIR"

	// SourceProject — ./.agents in the process working directory.
	SourceProject Source = "project:./.agents"

	// SourceUser — <user config dir>/mast/agents.
	SourceUser Source = "user:config-dir"

	// SourceSystem — /etc/mast/agents.
	SourceSystem Source = "system:/etc/mast/agents"
)

// Root is the selected `.agents/` root plus provenance.
type Root struct {
	// Dir is the absolute path of the selected root directory.
	Dir string

	// Source records which discovery rule matched.
	Source Source

	// Shadowed lists existing lower-priority candidate locations that
	// were NOT consulted because Dir won. Used for loud logging of the
	// "empty project dir shadows populated user dir" footgun.
	Shadowed []string
}

// systemAgentsDir is a var so tests can point it into a temp dir.
var systemAgentsDir = "/etc/mast/agents"

// Discover selects the `.agents/` root per the v0.1 discovery order.
// Exactly one location is used; see the package documentation.
func Discover() (Root, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Root{}, fmt.Errorf("config: getwd: %w", err)
	}
	// os.UserConfigDir can fail (e.g. no $HOME); treat that as "no user
	// candidate" rather than an error — it is a fallback location.
	userDir := ""
	if ucd, err := os.UserConfigDir(); err == nil {
		userDir = filepath.Join(ucd, "mast", "agents")
	}
	return discover(os.Getenv(EnvConfigDir), cwd, userDir, systemAgentsDir)
}

// discover is the testable core of Discover.
func discover(envDir, cwd, userDir, systemDir string) (Root, error) {
	if envDir != "" {
		if !dirExists(envDir) {
			return Root{}, fmt.Errorf(
				"config: %s=%q is set but is not an existing directory (when %s is set no other location is consulted)",
				EnvConfigDir, envDir, EnvConfigDir)
		}
		abs, err := filepath.Abs(envDir)
		if err != nil {
			return Root{}, fmt.Errorf("config: resolve %s=%q: %w", EnvConfigDir, envDir, err)
		}
		return Root{Dir: abs, Source: SourceEnv}, nil
	}

	type candidate struct {
		dir    string
		source Source
	}
	candidates := []candidate{
		{filepath.Join(cwd, ".agents"), SourceProject},
	}
	if userDir != "" {
		candidates = append(candidates, candidate{userDir, SourceUser})
	}
	candidates = append(candidates, candidate{systemDir, SourceSystem})

	for i, c := range candidates {
		if !dirExists(c.dir) {
			continue
		}
		root := Root{Dir: c.dir, Source: c.source}
		for _, later := range candidates[i+1:] {
			if dirExists(later.dir) {
				root.Shadowed = append(root.Shadowed, later.dir)
			}
		}
		return root, nil
	}

	consulted := make([]string, 0, len(candidates))
	for _, c := range candidates {
		consulted = append(consulted, c.dir)
	}
	return Root{}, fmt.Errorf(
		"config: no .agents root found (set %s or create one of: %v)",
		EnvConfigDir, consulted)
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
