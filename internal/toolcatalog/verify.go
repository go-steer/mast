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

package toolcatalog

import (
	"fmt"
	"sort"
	"strings"
)

// Wire is what a provider adapter actually put on the network, read
// back out of the captured request body: tool name -> the JSON schema
// object that tool's arguments were described by. Each adapter spells
// that field differently (Anthropic's input_schema, Gemini's
// parameters / parametersJsonSchema), so extracting it is the caller's
// job and comparing it is this package's.
type Wire map[string]map[string]any

// Verify compares what each tool's declaration promised against what
// the adapter emitted, and returns one human-readable line per
// violation — empty when the conversion was faithful.
//
// The comparison lives here, shared by every provider's test, so the
// adapters cannot drift in how strictly they are checked. A defect
// that one adapter is held to and another is not is how #154 survived
// a release: the Gemini path was fine, so nothing that ran against it
// would have caught the Anthropic converter dropping every schema.
//
// What it enforces, and why each one is a real outage rather than a
// cosmetic mismatch:
//
//   - Every catalogued tool is on the wire. A tool the model is never
//     shown cannot be called.
//   - A tool with arguments arrives with a non-empty properties map.
//     This is #154 exactly: the name survives, the schema does not, and
//     the model calls the tool with {} or does not call it at all.
//   - Every argument name survives, and no name is invented. A model
//     can only send arguments it was shown; one it was not shown is one
//     the tool will never receive.
//   - Every required argument is still marked required. Demoted to
//     optional, it becomes an argument the model may silently omit —
//     which surfaces as a runtime handler error, far from the cause.
//   - Each argument's own schema survives as a non-empty object. A
//     converter that lists the names but empties their types leaves the
//     model guessing whether "limit" is a number or a sentence.
func Verify(catalog []Entry, wire Wire) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	for _, e := range catalog {
		schema, ok := wire[e.Name]
		if !ok {
			report("%s (%s, %s): not on the wire at all — the model is never shown this tool", e.Name, e.Rig, e.Spelling)
			continue
		}

		props, _ := schema["properties"].(map[string]any)
		if e.HasParams() && len(props) == 0 {
			report("%s (%s, %s): declares %v but the emitted schema has no properties — the model can only call it with {} (this is the #154 shape)",
				e.Name, e.Rig, e.Spelling, e.Props)
			continue
		}
		if !e.HasParams() && len(props) > 0 {
			report("%s (%s, %s): declares no arguments but the emitted schema invented %v",
				e.Name, e.Rig, e.Spelling, sortedKeys(props))
			continue
		}

		for _, want := range e.Props {
			sub, present := props[want]
			if !present {
				report("%s (%s, %s): argument %q is missing from the emitted schema — the model cannot send it (emitted %v)",
					e.Name, e.Rig, e.Spelling, want, sortedKeys(props))
				continue
			}
			if obj, ok := sub.(map[string]any); !ok || len(obj) == 0 {
				report("%s (%s, %s): argument %q survived as %v — its own schema was emptied, so the model is guessing at its type",
					e.Name, e.Rig, e.Spelling, want, sub)
			}
		}
		if extra := difference(sortedKeys(props), e.Props); len(extra) > 0 {
			report("%s (%s, %s): the emitted schema advertises %v, which the declaration does not — a model calling with those arguments gets an error from the handler",
				e.Name, e.Rig, e.Spelling, extra)
		}

		if missing := difference(e.Required, wireRequired(schema)); len(missing) > 0 {
			report("%s (%s, %s): required argument(s) %v are optional on the wire — the model may omit them and the tool fails at dispatch",
				e.Name, e.Rig, e.Spelling, missing)
		}
	}

	sort.Strings(problems)
	return problems
}

// wireRequired reads the emitted required list, tolerating both the
// []any a generic JSON round-trip produces and the []string a typed
// one does.
func wireRequired(schema map[string]any) []string {
	var out []string
	switch req := schema["required"].(type) {
	case []any:
		for _, v := range req {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, req...)
	}
	sort.Strings(out)
	return out
}

// difference returns the entries of want that are absent from have.
func difference(want, have []string) []string {
	set := make(map[string]struct{}, len(have))
	for _, h := range have {
		set[h] = struct{}{}
	}
	var out []string
	for _, w := range want {
		if _, ok := set[w]; !ok {
			out = append(out, w)
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Summary renders a catalog as one line per tool, for a failure
// message that says what was actually checked. A test that fails
// without it leaves the reader unable to tell a converter bug from a
// catalog that quietly stopped containing anything.
func Summary(catalog []Entry) string {
	var b strings.Builder
	for _, e := range catalog {
		fmt.Fprintf(&b, "  %-24s rig=%-16s spelling=%-21s args=%v required=%v\n",
			e.Name, e.Rig, e.Spelling, e.Props, e.Required)
	}
	return b.String()
}
