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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/genai"
	"gopkg.in/yaml.v3"
)

// Why `output_schema:` names a file instead of inlining the schema.
//
// A report schema is a contract between the specialist that produces it
// and every consumer that reads it — the coordinator, the notifier, and
// (from W4) the bounded path that must emit the same shape without a
// specialist involved at all. Inlining it in one specialist's
// frontmatter makes that contract private to that specialist by
// construction: thirteen copies that drift independently, and no file a
// consumer can point at. So the schema is a bundle asset and every
// specialist that emits a report references the same one.
//
// The reference is resolved relative to the .tmpl file's own directory,
// which makes a roster relocatable — `../schemas/finding.json` means the
// same thing whether the roster is a workload bundle's specialists/ dir
// or a shared config root's. Absolute paths are refused because they
// make a bundle non-portable, not because they are dangerous: a .tmpl
// already names the tools a specialist may call and writes its system
// prompt verbatim, so a bundle is a trust domain and confining its file
// reads would be theatre.

// schemaExts are the extensions loadOutputSchema will read. Both are
// parsed by the same YAML decoder — YAML is a JSON superset — but the
// extension is checked rather than ignored so a `finding.jsonc` or a
// stray `finding.txt` is a load error and not a silently empty schema.
var schemaExts = map[string]bool{".json": true, ".yaml": true, ".yml": true}

// loadOutputSchema resolves a spec's `output_schema:` reference against
// the .tmpl file's directory and returns the parsed, normalized,
// checked schema.
func loadOutputSchema(tmplPath, ref string) (*genai.Schema, string, error) {
	if filepath.IsAbs(ref) {
		return nil, "", fmt.Errorf("output_schema %q must be relative to the specialist file, not absolute — an absolute path makes the bundle non-portable", ref)
	}
	ext := strings.ToLower(filepath.Ext(ref))
	if !schemaExts[ext] {
		return nil, "", fmt.Errorf("output_schema %q has extension %q (want .json, .yaml or .yml)", ref, ext)
	}
	path := filepath.Join(filepath.Dir(tmplPath), filepath.FromSlash(ref))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("output_schema %q: %w", ref, err)
	}
	schema, err := decodeSchema(data)
	if err != nil {
		return nil, "", fmt.Errorf("output_schema %s: %w", path, err)
	}
	normalizeSchema(schema)
	if err := checkSchema(schema, "$"); err != nil {
		return nil, "", fmt.Errorf("output_schema %s: %w", path, err)
	}
	return schema, path, nil
}

// decodeSchema parses a JSON-Schema document into a *genai.Schema.
//
// The document is read as YAML (a JSON superset, so one path serves
// both extensions) and re-encoded to JSON so genai's own field tags do
// the mapping. The second hop is decoded with DisallowUnknownFields:
// `propertys:` or `require:` would otherwise produce a schema that
// parses cleanly and constrains nothing, which is the failure mode this
// whole workstream exists to remove.
func decodeSchema(data []byte) (*genai.Schema, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("document is empty")
	}
	buf, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-encode: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	var schema genai.Schema
	if err := dec.Decode(&schema); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &schema, nil
}

// normalizeSchema upper-cases every `type` in the tree.
//
// JSON Schema spells types lowercase (`"object"`); genai's Type is the
// Vertex enum and spells them upper (`"OBJECT"`). ADK's validator
// upper-cases defensively before comparing, but the schema also travels
// to the provider verbatim as a function declaration, where the enum
// casing is what is on the wire. Normalizing once at load means the two
// consumers cannot disagree.
func normalizeSchema(s *genai.Schema) {
	if s == nil {
		return
	}
	s.Type = genai.Type(strings.ToUpper(string(s.Type)))
	normalizeSchema(s.Items)
	for _, p := range s.Properties {
		normalizeSchema(p)
	}
	for _, a := range s.AnyOf {
		normalizeSchema(a)
	}
}

// supportedTypes is exactly the set ADK's output validator dispatches
// on. Anything else reaches its `default:` arm and fails at runtime,
// having already spent a model call.
var supportedTypes = map[genai.Type]bool{
	genai.TypeString:  true,
	genai.TypeInteger: true,
	genai.TypeNumber:  true,
	genai.TypeBoolean: true,
	genai.TypeArray:   true,
	genai.TypeObject:  true,
}

// checkSchema rejects, at load time, the shapes that would otherwise
// fail on a live turn. Each of these is reachable and none of them is
// obvious from reading the document:
//
//   - An untyped node. ADK's validator switches on Type alone, so
//     `anyOf` without a type — legal JSON Schema — is an unsupported
//     type at runtime.
//   - An array without items, which ADK errors on only once a value
//     arrives to be checked.
//   - A required name with no matching property. The model cannot
//     satisfy it, so a Task-mode specialist retries until it runs out
//     of turns and a SingleTurn one fails every reply.
//
// The top level must be an object, and that one is not a style
// preference: ADK's two modes disagree about non-object schemas. Task
// mode wraps a scalar under a `result` key and unwraps it again, while
// SingleTurn's ValidateOutputSchema unmarshals the reply into
// map[string]any before it looks at the schema at all, so a scalar
// top-level schema fails there unconditionally. A schema that means
// different things in the two modes is a trap; a report contract is an
// object anyway.
func checkSchema(s *genai.Schema, path string) error {
	if s == nil {
		return fmt.Errorf("%s: null schema", path)
	}
	if s.Type == "" {
		return fmt.Errorf("%s: no type — every node needs one, because output validation dispatches on it and has no other arm", path)
	}
	if !supportedTypes[s.Type] {
		return fmt.Errorf("%s: type %q is not one of object, array, string, integer, number, boolean", path, s.Type)
	}
	if path == "$" && s.Type != genai.TypeObject {
		return fmt.Errorf("$: top-level type must be object, got %q — a SingleTurn specialist's reply is unmarshalled into an object before the schema is consulted, so a scalar contract can never validate there", s.Type)
	}
	switch s.Type {
	case genai.TypeArray:
		if s.Items == nil {
			return fmt.Errorf("%s: array has no items schema", path)
		}
		if err := checkSchema(s.Items, path+"[]"); err != nil {
			return err
		}
	case genai.TypeObject:
		if len(s.Properties) == 0 {
			return fmt.Errorf("%s: object has no properties — it would accept nothing, since validation refuses any key the schema does not name", path)
		}
		for _, name := range sortedKeys(s.Properties) {
			if err := checkSchema(s.Properties[name], path+"."+name); err != nil {
				return err
			}
		}
		for _, req := range s.Required {
			if _, ok := s.Properties[req]; !ok {
				return fmt.Errorf("%s: required name %q is not a property (have: %s)", path, req, strings.Join(sortedKeys(s.Properties), ", "))
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]*genai.Schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
