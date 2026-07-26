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
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadDir reads every *.tmpl file in dir non-recursively and parses each
// into a Spec. Results are returned sorted by Spec.Name for deterministic
// ordering.
func LoadDir(dir string) ([]Spec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("specialists: read dir %q: %w", dir, err)
	}
	var specs []Spec
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".tmpl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		spec, err := LoadFile(path)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, nil
}

// LoadFile reads and parses a single .tmpl file.
func LoadFile(path string) (Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("specialists: read %q: %w", path, err)
	}
	fm, body, err := splitFrontmatter(data)
	if err != nil {
		return Spec{}, fmt.Errorf("specialists: %q: %w", path, err)
	}
	name := fm.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".tmpl")
	}
	if fm.Description == "" {
		return Spec{}, fmt.Errorf("specialists: %q: description is required", path)
	}
	mode := fm.Mode
	if mode == "" {
		mode = ModeTask
	}
	if mode != ModeTask && mode != ModeSingleTurn {
		return Spec{}, fmt.Errorf("specialists: %q: unknown mode %q (want Task or SingleTurn)", path, mode)
	}
	return Spec{
		Filename:    path,
		Name:        name,
		Description: fm.Description,
		Mode:        mode,
		Model:       fm.Model,
		Budget:      fm.Budget,
		Tools:       fm.Tools,
		Instruction: strings.TrimSpace(body),
	}, nil
}

// splitFrontmatter separates a `---\n<yaml>\n---\n<body>` document. A
// missing frontmatter block is an error — every .tmpl must declare
// at minimum its description.
func splitFrontmatter(data []byte) (Frontmatter, string, error) {
	const sep = "---"

	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte(sep)) {
		return Frontmatter{}, "", fmt.Errorf("missing frontmatter (file must start with `---`)")
	}
	// Consume the opening `---` line.
	rest := trimmed[len(sep):]
	// Require the opening line to end at a newline.
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return Frontmatter{}, "", fmt.Errorf("missing frontmatter terminator (no `---` on its own line)")
	}
	rest = rest[nl+1:]

	// Find the closing `---` line.
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return Frontmatter{}, "", fmt.Errorf("missing frontmatter terminator (no closing `---`)")
	}
	yamlBlock := rest[:end]
	body := rest[end+len("\n---"):]
	// Skip the newline after the closing `---`.
	if len(body) > 0 && body[0] == '\n' {
		body = body[1:]
	}

	var fm Frontmatter
	if err := yaml.Unmarshal(yamlBlock, &fm); err != nil {
		return Frontmatter{}, "", fmt.Errorf("parse frontmatter yaml: %w", err)
	}
	return fm, string(body), nil
}
