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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/genai"
	"gopkg.in/yaml.v3"

	"github.com/go-steer/mast/pkg/taskclass"
)

// Extension is the specialist file extension. These files are YAML
// frontmatter plus a Markdown body and have never been Go templates —
// nothing substitutes into them, and a `{{ ... }}` in one is refused
// rather than interpolated (#272, checkPlaceholders below). The name
// says what they are, and editors highlight them correctly (#292).
const Extension = ".specialist.md"

// LegacyExtension is what specialist files were called through v0.8.
// Still loaded, with a deprecation warning, for one release: an
// out-of-tree bundle is exactly the thing this project tells people to
// write, so the rename cannot be a flag day. Removal is #349 on the
// v0.9 milestone rather than a promise in this comment, so it can go
// stale visibly.
const LegacyExtension = ".tmpl"

// specialistName splits a specialist filename into its stem and which
// extension it used. ok is false for a file that is neither.
func specialistName(base string) (stem string, legacy, ok bool) {
	switch {
	case strings.HasSuffix(base, Extension):
		return strings.TrimSuffix(base, Extension), false, true
	case strings.HasSuffix(base, LegacyExtension):
		return strings.TrimSuffix(base, LegacyExtension), true, true
	}
	return base, false, false
}

// LoadDir reads every specialist file in dir non-recursively and parses
// each into a Spec. Both Extension and LegacyExtension are accepted.
// Results are returned sorted by Spec.Name for deterministic ordering.
//
// A stem defined under both extensions is refused. During the rename
// the realistic mistake is a copy left behind, and the two files are
// the same specialist under any reading — so which one wins would be an
// alphabetical accident, and the stale half would keep running.
func LoadDir(dir string) ([]Spec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("specialists: read dir %q: %w", dir, err)
	}
	var specs []Spec
	seen := map[string]string{} // stem -> filename it was first seen as
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		stem, _, ok := specialistName(e.Name())
		if !ok {
			continue
		}
		if prev, dup := seen[stem]; dup {
			return nil, fmt.Errorf(
				"specialists: %q in %s is defined by both %s and %s — these are the same specialist under two extensions; delete the %s one (%s is deprecated, see #292)",
				stem, dir, prev, e.Name(), LegacyExtension, LegacyExtension)
		}
		seen[stem] = e.Name()
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

// WarnLegacyExtension logs one deprecation warning naming every spec
// still loaded from a LegacyExtension file, and nothing when there are
// none. It lives here so both callers with a logger — pkg/config's
// root load and cmd/mast's path mode — say the same thing; LoadDir
// itself stays log-free so an embedder decides where this goes.
func WarnLegacyExtension(logger *slog.Logger, specs []Spec) {
	if logger == nil {
		return
	}
	var stale []string
	for _, s := range specs {
		if s.LegacyExtension {
			stale = append(stale, filepath.Base(s.Filename))
		}
	}
	if len(stale) == 0 {
		return
	}
	sort.Strings(stale)
	logger.Warn("specialist files still use the deprecated "+LegacyExtension+" extension",
		"files", strings.Join(stale, ", "),
		"count", len(stale),
		"rename_to", "<name>"+Extension,
		"accepted_through", "v0.8",
		"issue", "https://github.com/go-steer/mast/issues/292")
}

// LoadFile reads and parses a single specialist file.
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
	stem, legacy, _ := specialistName(filepath.Base(path))
	name := fm.Name
	if name == "" {
		name = stem
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
		LegacyExtension:  legacy,
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
// missing frontmatter block is an error — every specialist file must declare
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

	// Strict, for the reason the bundle loader is strict (#302): a key
	// this loader does not recognise is a key that silently does
	// nothing, and here one of them does nothing in the unsafe
	// direction. ToolAllowlist.InheritsAllMCP() is `MCP == nil`, so a
	// misspelled `tools:` does not narrow the specialist to the servers
	// its file lists — it hands over every MCP server the workload
	// wires, and nothing downstream can distinguish that from an author
	// who meant to inherit. A refused file naming the key is strictly
	// better. (`capability:` is the milder case: absent resolves to
	// read_only, so misspelling it fails toward the safe value.)
	//
	// Deliberately NO schema_version here, unlike the bundle. A
	// specialist is only ever reached through a bundle that names it,
	// so the bundle's version already governs the roster; a second,
	// independently-versioned artifact would multiply the compatibility
	// matrix by 39 files for a document that is mostly prose. If the
	// frontmatter ever needs a breaking change, it rides the bundle's
	// version bump — recorded in docs/README.md's resolved decisions.
	var fm Frontmatter
	dec := yaml.NewDecoder(bytes.NewReader(yamlBlock))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil && !errors.Is(err, io.EOF) {
		return Frontmatter{}, "", fmt.Errorf("parse frontmatter yaml: %w", err)
	}
	return fm, string(body), nil
}
