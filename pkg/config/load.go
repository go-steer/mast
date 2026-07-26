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
	"sort"
	"strconv"
	"strings"

	"github.com/go-steer/mast/pkg/a2a"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// Env var names for the v0.1 scalar workload-budget overrides. See the
// package documentation for the key→env mapping convention.
const (
	EnvBudgetMaxCostUSD          = "MAST_BUDGET_MAX_COST_USD"
	EnvBudgetMaxWallclockSeconds = "MAST_BUDGET_MAX_WALLCLOCK_SECONDS"
)

// Config is the loaded contents of the selected `.agents/` root.
type Config struct {
	// Root is the selected root plus provenance.
	Root Root

	// Workloads maps workload name → loaded bundle
	// (<root>/workloads/*.yaml, flat scan).
	Workloads map[string]workload.Bundle

	// Specialists maps specialist name → loaded spec
	// (<root>/specialists/*.tmpl, flat scan).
	Specialists map[string]specialists.Spec

	// A2A maps remote-agent name → static A2A registration
	// (<root>/a2a/*.yaml, flat scan). Consumed by a2a.NewAdapter for
	// the federation registry's a2a:// scheme.
	A2A map[string]a2a.AgentConfig
}

// A2AList returns the loaded A2A registrations in name order — the
// shape a2a.NewAdapter takes.
func (c *Config) A2AList() []a2a.AgentConfig {
	out := make([]a2a.AgentConfig, 0, len(c.A2A))
	for _, name := range sortedKeys(c.A2A) {
		out = append(out, c.A2A[name])
	}
	return out
}

// Load discovers the `.agents/` root and loads it. Equivalent to
// Discover followed by LoadRoot.
func Load(logger *slog.Logger) (*Config, error) {
	root, err := Discover()
	if err != nil {
		return nil, err
	}
	return LoadRoot(root, logger)
}

// LoadRoot loads workloads and specialists from the given root,
// applies env-var budget overrides, and cross-validates workload
// specialist rosters against the loaded specialist set. All errors are
// fatal load-time errors per config-layout-design.md v0.1.
//
// LoadRoot logs loudly — selected root, provenance, per-directory
// findings, shadowed lower-priority locations — because exclusive
// single-location discovery means an empty selected root silently
// shadows a populated one elsewhere.
func LoadRoot(root Root, logger *slog.Logger) (*Config, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg := &Config{Root: root}

	var err error
	if cfg.Workloads, err = loadWorkloads(filepath.Join(root.Dir, "workloads")); err != nil {
		return nil, err
	}
	if cfg.Specialists, err = loadSpecialists(filepath.Join(root.Dir, "specialists")); err != nil {
		return nil, err
	}
	if cfg.A2A, err = loadA2A(filepath.Join(root.Dir, "a2a")); err != nil {
		return nil, err
	}
	if err := applyBudgetEnvOverrides(cfg.Workloads, logger); err != nil {
		return nil, err
	}

	// Cross-file load-time validation: every workload's specialist
	// roster must resolve against the loaded specialist set.
	for _, name := range sortedKeys(cfg.Workloads) {
		b := cfg.Workloads[name]
		for _, ref := range b.Specialists {
			if _, ok := cfg.Specialists[ref]; !ok {
				return nil, fmt.Errorf(
					"config: workload %q (%s) references specialist %q not found in %s",
					name, b.Filename, ref, filepath.Join(root.Dir, "specialists"))
			}
		}
	}

	logConfig(cfg, logger)
	return cfg, nil
}

// loadWorkloads enumerates dir/*.yaml and dir/*.yml (flat,
// non-recursive) via pkg/workload. A missing dir yields zero entries.
// Two files resolving to the same workload name are fatal.
func loadWorkloads(dir string) (map[string]workload.Bundle, error) {
	out := map[string]workload.Bundle{}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read workloads dir %q: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := workload.Load(path)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if prev, ok := out[b.Name]; ok {
			return nil, fmt.Errorf(
				"config: workload name collision: %q defined by both %s and %s (same-directory collisions are fatal in v0.1; rename one)",
				b.Name, prev.Filename, path)
		}
		out[b.Name] = b
	}
	return out, nil
}

// loadSpecialists loads dir/*.tmpl (flat, non-recursive) via
// pkg/specialists and rejects same-name collisions. A missing dir
// yields zero entries.
func loadSpecialists(dir string) (map[string]specialists.Spec, error) {
	out := map[string]specialists.Spec{}
	if !dirExists(dir) {
		return out, nil
	}
	specs, err := specialists.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for _, s := range specs {
		if prev, ok := out[s.Name]; ok {
			return nil, fmt.Errorf(
				"config: specialist name collision: %q defined by both %s and %s (same-directory collisions are fatal in v0.1; rename one)",
				s.Name, prev.Filename, s.Filename)
		}
		out[s.Name] = s
	}
	return out, nil
}

// loadA2A loads dir/*.yaml and dir/*.yml (flat, non-recursive) via
// pkg/a2a and rejects same-name collisions. A missing dir yields zero
// entries.
func loadA2A(dir string) (map[string]a2a.AgentConfig, error) {
	out := map[string]a2a.AgentConfig{}
	cfgs, err := a2a.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for _, c := range cfgs {
		if prev, ok := out[c.Name]; ok {
			return nil, fmt.Errorf(
				"config: a2a agent name collision: %q defined by both %s and %s (same-directory collisions are fatal in v0.1; rename one)",
				c.Name, prev.Filename, c.Filename)
		}
		out[c.Name] = c
	}
	return out, nil
}

// applyBudgetEnvOverrides applies the MAST_BUDGET_* scalar overrides
// to every loaded workload bundle. Env overrides file values
// unconditionally; a set-but-unparseable value is fatal. An
// empty-string value is treated as unset (same as MAST_CONFIG_DIR).
func applyBudgetEnvOverrides(workloads map[string]workload.Bundle, logger *slog.Logger) error {
	if raw := os.Getenv(EnvBudgetMaxCostUSD); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 {
			return fmt.Errorf("config: %s=%q: want a non-negative float", EnvBudgetMaxCostUSD, raw)
		}
		for name, b := range workloads {
			b.Budget.MaxCostUSD = v
			workloads[name] = b
		}
		logger.Info("env override applied", "var", EnvBudgetMaxCostUSD, "value", v, "workloads", len(workloads))
	}
	if raw := os.Getenv(EnvBudgetMaxWallclockSeconds); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			return fmt.Errorf("config: %s=%q: want a non-negative integer", EnvBudgetMaxWallclockSeconds, raw)
		}
		for name, b := range workloads {
			b.Budget.MaxWallclockSeconds = v
			workloads[name] = b
		}
		logger.Info("env override applied", "var", EnvBudgetMaxWallclockSeconds, "value", v, "workloads", len(workloads))
	}
	return nil
}

// logConfig is the loud selection report mandated by the spike-2
// resolved decision: exclusive discovery means an empty selected root
// shadows populated lower-priority roots, so always say what was
// selected, why, and what it contains.
func logConfig(cfg *Config, logger *slog.Logger) {
	logger.Info("config root selected",
		"dir", cfg.Root.Dir,
		"source", string(cfg.Root.Source),
		"workloads", len(cfg.Workloads),
		"specialists", len(cfg.Specialists),
		"a2a_agents", len(cfg.A2A),
		"workload_names", strings.Join(sortedKeys(cfg.Workloads), ","),
		"specialist_names", strings.Join(sortedKeys(cfg.Specialists), ","),
		"a2a_agent_names", strings.Join(sortedKeys(cfg.A2A), ","),
	)
	for _, shadowed := range cfg.Root.Shadowed {
		logger.Warn("lower-priority config location exists but is IGNORED (exclusive single-location discovery; no cross-location merging)",
			"selected", cfg.Root.Dir, "ignored", shadowed)
	}
	if len(cfg.Workloads) == 0 && len(cfg.Specialists) == 0 && len(cfg.A2A) == 0 {
		logger.Warn("selected config root is EMPTY — if you expected workloads/specialists from another location, note that the selected root shadows it outright",
			"dir", cfg.Root.Dir, "source", string(cfg.Root.Source))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
