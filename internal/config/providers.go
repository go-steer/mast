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
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/go-steer/core-models/profile"
	"gopkg.in/yaml.v3"
)

// loadProviders enumerates dir/*.yaml and dir/*.yml (flat,
// non-recursive): one provider profile per file, in core-models'
// profile schema (docs/model-support-design.md, Resolved decisions —
// OQ-1). A missing dir yields zero entries. An unknown key is a load
// error rather than a silently missing value, the same strictness
// bundles get (#302); two files declaring the same name are fatal.
//
// Shape only: a profile is checked against the environment (variables
// set, credentials found) when it is opened, so a profile nobody
// selects never fails a run for want of its API key.
func loadProviders(dir string) (map[string]profile.Profile, error) {
	out := map[string]profile.Profile{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", dir, err)
	}
	files := map[string]string{}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		p, err := loadProviderFile(path)
		if err != nil {
			return nil, err
		}
		if prev, ok := files[p.Name]; ok {
			return nil, fmt.Errorf("config: provider profile %q is defined by both %s and %s", p.Name, prev, path)
		}
		files[p.Name] = path
		out[p.Name] = p
	}
	return out, nil
}

func loadProviderFile(path string) (profile.Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return profile.Profile{}, fmt.Errorf("config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var p profile.Profile
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return profile.Profile{}, fmt.Errorf("config: provider profile %s: %w", path, err)
	}
	if p.Name == "" {
		return profile.Profile{}, fmt.Errorf("config: provider profile %s has no name", path)
	}
	expanded, err := profile.Expand(p)
	if err == nil {
		err = expanded.Validate()
	}
	if err != nil {
		return profile.Profile{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return p, nil
}

// ProvidersList returns the loaded profiles in name order.
func (c *Config) ProvidersList() []profile.Profile {
	out := make([]profile.Profile, 0, len(c.Providers))
	for _, name := range sortedKeys(c.Providers) {
		out = append(out, c.Providers[name])
	}
	return out
}

// DiscoverProviders loads only the provider profiles of the discovered
// `.agents/` root, for the CLI, which must know them before it can
// validate --provider — earlier than, and independent of, loading a
// workload. No root at all is not an error here: it means no declared
// profiles, and core-models' built-in ones are still selectable.
func DiscoverProviders(logger *slog.Logger) ([]profile.Profile, error) {
	root, err := Discover()
	if err != nil {
		if errors.Is(err, ErrNoRoot) {
			return nil, nil
		}
		return nil, err
	}
	m, err := loadProviders(filepath.Join(root.Dir, "providers"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m))
	out := make([]profile.Profile, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, m[n])
	}
	if logger != nil && len(out) > 0 {
		logger.Info("provider profiles loaded", "dir", filepath.Join(root.Dir, "providers"), "profiles", names)
	}
	return out, nil
}
