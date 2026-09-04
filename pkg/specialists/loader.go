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

	"google.golang.org/genai"
	"gopkg.in/yaml.v3"

	"github.com/go-steer/mast/pkg/taskclass"
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
	// Before anything else about the body: braces in it are ADK's, not
	// the author's. Refused here, where the file is open and the line
	// number is known, rather than on the first run of the specialist,
	// where the error names neither (#272, placeholders.go).
	if err := checkPlaceholders(path, body); err != nil {
		return Spec{}, err
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
	capability := fm.Capability
	if capability == "" {
		capability = CapabilityReadOnly
	}
	if capability != CapabilityReadOnly && capability != CapabilityChangeExecutor {
		// Refused rather than defaulted: a misspelled capability is a
		// write declaration that did not take, and defaulting it to
		// read_only would fail the roster somewhere far from the typo.
		return Spec{}, fmt.Errorf("specialists: %q: unknown capability %q (want read_only or change_executor)", path, capability)
	}
	if fm.Tier != "" {
		// Same reasoning as the capability refusal above: a misspelled
		// tier is a model choice that did not take. Defaulting it to
		// the parent's model would run twelve diagnosers on the
		// frontier model and only show up on the bill.
		switch fm.Tier {
		case taskclass.TierSmall, taskclass.TierMid, taskclass.TierFrontier:
		default:
			return Spec{}, fmt.Errorf("specialists: %q: unknown tier %q (want %s, %s or %s)",
				path, fm.Tier, taskclass.TierSmall, taskclass.TierMid, taskclass.TierFrontier)
		}
		if fm.Model != "" {
			return Spec{}, fmt.Errorf("specialists: %q: declares both model %q and tier %q (use one: model: pins an exact ID, tier: resolves per provider)",
				path, fm.Model, fm.Tier)
		}
	}
	if len(fm.Tools.Skills) > 0 {
		// Same reasoning as the two refusals above, one step further
		// out: this is a tool grant that cannot take, because mast
		// ships no skills runtime for it to narrow. There is no
		// pkg/skills in this fork, no loader, no invoke_skill — the
		// axis is documented in docs/specialists-design.md and
		// scheduled by docs/skills-design.md, and neither has landed
		// through v0.4 (#211).
		//
		// Accepting it would make an allowlist that grants nothing
		// read exactly like one that grants three things, on the one
		// file in the tree whose whole job is to say what a
		// sub-agent may touch. `skills: []` is still accepted: on
		// every axis a present-but-empty list means deny-all, and
		// deny-all is what mast actually does here.
		return Spec{}, fmt.Errorf("specialists: %q: tools.skills lists %d skill(s), and this build has no skills subsystem to grant them from — the field would be silently inert (remove it, or write `skills: []` to state deny-all explicitly)",
			path, len(fm.Tools.Skills))
	}
	var schema *genai.Schema
	var schemaPath string
	if fm.OutputSchema != "" {
		// Loaded here rather than at Build time so that a bundle with a
		// malformed contract fails to load, not to run. The difference
		// matters at 3am: a load error names the file on startup, a
		// build error surfaces on the first turn that dispatches to
		// this specialist.
		schema, schemaPath, err = loadOutputSchema(path, fm.OutputSchema)
		if err != nil {
			return Spec{}, fmt.Errorf("specialists: %q: %w", path, err)
		}
	}
	return Spec{
		Filename:         path,
		Name:             name,
		Description:      fm.Description,
		Mode:             mode,
		Model:            fm.Model,
		Tier:             fm.Tier,
		Capability:       capability,
		Budget:           fm.Budget,
		Tools:            fm.Tools,
		Instruction:      strings.TrimSpace(body),
		OutputSchema:     schema,
		OutputSchemaPath: schemaPath,
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
