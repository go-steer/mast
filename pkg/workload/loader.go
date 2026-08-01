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
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads a workload bundle YAML file, parses it, validates required
// fields, and returns the populated Bundle.
func Load(path string) (Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, fmt.Errorf("workload: read %q: %w", path, err)
	}
	var b Bundle
	if err := yaml.Unmarshal(data, &b); err != nil {
		return Bundle{}, fmt.Errorf("workload: parse %q: %w", path, err)
	}
	b.Filename = path
	if err := b.validate(); err != nil {
		return Bundle{}, fmt.Errorf("workload: validate %q: %w", path, err)
	}
	if b.Mode == "" {
		b.Mode = ModeSingleSession
	}
	return b, nil
}

func (b *Bundle) validate() error {
	if b.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(b.Specialists) == 0 {
		return fmt.Errorf("specialists roster is empty (workload cannot dispatch)")
	}
	if b.Mode != "" && b.Mode != ModeSingleSession && b.Mode != ModeMultiSession {
		return fmt.Errorf("unknown mode %q (want single_session or multi_session)", b.Mode)
	}
	seen := make(map[string]bool, len(b.Specialists))
	for _, name := range b.Specialists {
		if name == "" {
			return fmt.Errorf("specialists roster contains an empty entry")
		}
		if seen[name] {
			return fmt.Errorf("specialists roster contains duplicate %q", name)
		}
		seen[name] = true
	}
	seenTools := make(map[string]bool, len(b.ToolCatalog.Tools))
	for _, p := range b.ToolCatalog.Tools {
		if p.Name == "" {
			return fmt.Errorf("tool_catalog.tools contains an entry without a name")
		}
		if seenTools[p.Name] {
			return fmt.Errorf("tool_catalog.tools contains duplicate %q", p.Name)
		}
		seenTools[p.Name] = true
	}
	return nil
}
