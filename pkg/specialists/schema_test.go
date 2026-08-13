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

package specialists_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/specialists"
)

// tmplWithSchema is a minimal spec that points at ../schemas/<ref>.
func tmplWithSchema(ref string) string {
	return "---\ndescription: Emits a finding.\noutput_schema: " + ref + "\n---\nbody\n"
}

const findingJSON = `{
  "type": "object",
  "properties": {
    "summary": {"type": "string"},
    "severity": {"type": "string"},
    "evidence": {
      "type": "array",
      "items": {"type": "string"}
    }
  },
  "required": ["summary", "severity"]
}
`

const findingYAML = `type: object
properties:
  summary:
    type: string
  severity:
    type: string
required:
  - summary
  - severity
`

// writeBundle lays out the shape a real workload has — a specialists/
// dir next to a schemas/ dir — and returns the .tmpl path. The layout is
// part of what is under test: `../schemas/x.json` has to resolve against
// the .tmpl file's own directory, not the process's cwd, or a roster
// stops being relocatable.
func writeBundle(t *testing.T, ref string, schema string) string {
	t.Helper()
	root := t.TempDir()
	specDir := filepath.Join(root, "specialists")
	schemaDir := filepath.Join(root, "schemas")
	for _, d := range []string{specDir, schemaDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if schema != "" {
		name := filepath.Base(ref)
		if err := os.WriteFile(filepath.Join(schemaDir, name), []byte(schema), 0o600); err != nil {
			t.Fatalf("write schema: %v", err)
		}
	}
	path := filepath.Join(specDir, "diagnoser.tmpl")
	if err := os.WriteFile(path, []byte(tmplWithSchema(ref)), 0o600); err != nil {
		t.Fatalf("write tmpl: %v", err)
	}
	return path
}

// TestLoadFile_OutputSchemaJSON is the happy path, and it asserts the
// normalization too: the document spells its types the JSON-Schema way
// (lowercase) and the loaded schema must spell them the genai way, all
// the way down. A nested type left lowercase validates fine against
// ADK's defensively upper-casing validator and then goes on the wire as
// an unrecognized enum.
func TestLoadFile_OutputSchemaJSON(t *testing.T) {
	path := writeBundle(t, "../schemas/finding.json", findingJSON)

	spec, err := specialists.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if spec.OutputSchema == nil {
		t.Fatal("OutputSchema is nil")
	}
	if got := spec.OutputSchema.Type; got != genai.TypeObject {
		t.Errorf("top-level type = %q, want %q", got, genai.TypeObject)
	}
	if got := spec.OutputSchema.Properties["summary"].Type; got != genai.TypeString {
		t.Errorf("summary type = %q, want %q", got, genai.TypeString)
	}
	if got := spec.OutputSchema.Properties["evidence"].Items.Type; got != genai.TypeString {
		t.Errorf("evidence item type = %q, want %q — normalization did not reach array items", got, genai.TypeString)
	}
	want := filepath.Join(filepath.Dir(filepath.Dir(path)), "schemas", "finding.json")
	if spec.OutputSchemaPath != want {
		t.Errorf("OutputSchemaPath = %q, want %q", spec.OutputSchemaPath, want)
	}
}

// TestLoadFile_OutputSchemaYAML pins that .yaml is a first-class
// spelling and not an accident of the decoder.
func TestLoadFile_OutputSchemaYAML(t *testing.T) {
	path := writeBundle(t, "../schemas/finding.yaml", findingYAML)

	spec, err := specialists.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if spec.OutputSchema == nil || spec.OutputSchema.Type != genai.TypeObject {
		t.Fatalf("OutputSchema = %#v, want an object schema", spec.OutputSchema)
	}
	if _, ok := spec.OutputSchema.Properties["severity"]; !ok {
		t.Errorf("properties = %v, want severity", spec.OutputSchema.Properties)
	}
}

// TestLoadFile_OutputSchemaRejects covers every shape the loader is
// there to catch. Each case is a document that either parses cleanly and
// constrains nothing, or fails only once a live turn has already spent a
// model call — which is precisely why they are worth a load-time error.
func TestLoadFile_OutputSchemaRejects(t *testing.T) {
	cases := []struct {
		name   string
		ref    string
		schema string
		// want is a distinctive fragment of the error. Asserting on it
		// keeps the cases honest: several of these documents are wrong
		// in more than one way, and a bare "err != nil" would let the
		// wrong check take the credit.
		want string
	}{{
		name:   "absolute path",
		ref:    "/etc/finding.json",
		schema: findingJSON,
		want:   "must be relative",
	}, {
		name:   "unknown extension",
		ref:    "../schemas/finding.txt",
		schema: findingJSON,
		want:   "extension",
	}, {
		name:   "missing file",
		ref:    "../schemas/absent.json",
		schema: "",
		want:   "no such file",
	}, {
		name:   "empty document",
		ref:    "../schemas/finding.json",
		schema: "",
		want:   "is empty",
	}, {
		name:   "misspelled key",
		ref:    "../schemas/finding.json",
		schema: `{"type": "object", "propertys": {"summary": {"type": "string"}}}`,
		want:   "propertys",
	}, {
		name:   "untyped node",
		ref:    "../schemas/finding.json",
		schema: `{"type": "object", "properties": {"summary": {"description": "no type"}}}`,
		want:   "$.summary: no type",
	}, {
		name:   "unsupported type",
		ref:    "../schemas/finding.json",
		schema: `{"type": "object", "properties": {"summary": {"type": "null"}}}`,
		want:   `$.summary: type "NULL" is not one of`,
	}, {
		name:   "non-object top level",
		ref:    "../schemas/finding.json",
		schema: `{"type": "string"}`,
		want:   "top-level type must be object",
	}, {
		name:   "array without items",
		ref:    "../schemas/finding.json",
		schema: `{"type": "object", "properties": {"evidence": {"type": "array"}}}`,
		want:   "$.evidence: array has no items",
	}, {
		name:   "object with no properties",
		ref:    "../schemas/finding.json",
		schema: `{"type": "object"}`,
		want:   "has no properties",
	}, {
		name:   "required name is not a property",
		ref:    "../schemas/finding.json",
		schema: `{"type": "object", "properties": {"summary": {"type": "string"}}, "required": ["severity"]}`,
		want:   `required name "severity" is not a property`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The empty-document case needs the file to exist and be
			// empty; the missing-file case needs it absent. writeBundle
			// skips the write on "", so spell the empty file out here.
			path := writeBundle(t, tc.ref, tc.schema)
			if tc.name == "empty document" {
				p := filepath.Join(filepath.Dir(filepath.Dir(path)), "schemas", "finding.json")
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatalf("write empty: %v", err)
				}
			}

			_, err := specialists.LoadFile(path)
			if err == nil {
				t.Fatalf("LoadFile accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			// Every load error names the specialist file, because the
			// operator reading it at 3am has a roster, not a stack.
			if !strings.Contains(err.Error(), "diagnoser.tmpl") {
				t.Errorf("error does not name the specialist file: %v", err)
			}
		})
	}
}

// TestLoadFile_NoOutputSchema pins that the field stays optional. Most
// specialists have no report contract and must keep loading without one.
func TestLoadFile_NoOutputSchema(t *testing.T) {
	dir := t.TempDir()
	writeTempTmpl(t, dir, "plain.tmpl", "---\ndescription: No contract.\n---\nbody\n")

	spec, err := specialists.LoadFile(filepath.Join(dir, "plain.tmpl"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if spec.OutputSchema != nil || spec.OutputSchemaPath != "" {
		t.Errorf("OutputSchema = %#v / %q, want both empty", spec.OutputSchema, spec.OutputSchemaPath)
	}
}
