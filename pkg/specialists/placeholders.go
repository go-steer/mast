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

// Braces in a prompt are not punctuation (#272).
//
// ADK scans every instruction for `{...}` and resolves each hit before
// the prompt is sent: a bare identifier in braces is a session-state
// lookup, and `artifact.`-prefixed one is an artifact load. A template
// that says "the project is {project}" is therefore asking for a state
// key named project, and when nothing set one the run dies with
//
//	failed to inject session state into instruction: state key does not exist
//
// which names neither the template nor the placeholder. That is a bad
// error for a good rule, and the rule is invisible: template authors
// write prompts full of Kubernetes and GCP examples, which is exactly
// where braces live.
//
// Worse, there is no escape. The regex matches runs of braces, so
// doubling them changes nothing — `{{project}}` is trimmed to the same
// key. Nor does whitespace help: the key is trimmed before it is looked
// up. A bare identifier in braces is a lookup, always.
//
// So the check is at load, where the file is open and the line number
// is known. It refuses the same templates ADK would have failed on, and
// it says which one, on what line, and what to write instead.
//
// # What it accepts
//
// Only what ADK resolves. `{"replicas": 1}` in a JSON example is not a
// lookup — the key is not an identifier, and ADK hands those back
// verbatim — so a template full of manifests is fine. Neither is
// `{app: web}`: the space makes it a non-identifier too. `{app:web}`
// without the space IS one, `app:` being one of ADK's three state
// scopes, which is the asymmetry worth knowing about before it costs
// someone an afternoon.
//
// A state placeholder marked optional is accepted, because that marker
// is what makes state injection safe: `{project?}` resolves to the key
// when it is set and to nothing when it is not, and cannot fail the
// run. A template that genuinely wants session state injected says so
// that way.
//
// Artifacts get no such reprieve. ADK checks for an artifact service
// before it consults the optional marker, and mast runs none, so
// `{artifact.report?}` fails exactly as `{artifact.report}` does. For
// those the only fix is to stop writing it in braces.

package specialists

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// placeholderRegex is ADK's, copied rather than imported: it lives in
// google.golang.org/adk/v2/internal/llminternal, which no package
// outside ADK can reach. TestThePlaceholderRuleIsStillADKs pins this
// copy against that source, so a divergence shows up as a failing test
// rather than as a template mast accepted and ADK died on.
var placeholderRegex = regexp.MustCompile(`{+[^{}]*}+`)

// statePrefixes are the three qualified state scopes ADK recognises.
// Anything else before a colon makes the whole name invalid, and
// therefore literal.
var statePrefixes = []string{"app:", "user:", "temp:"}

// artifactPrefix marks an artifact load rather than a state lookup.
const artifactPrefix = "artifact."

// badPlaceholder is one lookup a template asked for that will fail the
// first time the specialist runs.
type badPlaceholder struct {
	// text is the placeholder exactly as written, braces included, so
	// the author can find it by searching for it.
	text string
	// key is what ADK resolves after trimming the braces and the
	// optional marker.
	key string
	// artifact distinguishes the two failures, which have different
	// fixes: a state lookup can be made optional, an artifact load
	// cannot.
	artifact bool
	line     int
}

// checkPlaceholders reports every placeholder in a template body that
// ADK will try to resolve and fail on, as one error naming all of them.
//
// All of them rather than the first: an author who fixes one and
// restarts to find the next has learned the rule the slowest possible
// way, and a prompt with braces in it usually has several.
func checkPlaceholders(path, body string) error {
	bad := resolvablePlaceholders(body)
	if len(bad) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "specialists: %q: %s in the template body %s resolved before the prompt is sent, not sent literally",
		path, plural(len(bad), "placeholder"), verb(len(bad)))
	states, artifacts := 0, 0
	for _, p := range bad {
		what := fmt.Sprintf("looks up session-state key %q", p.key)
		if p.artifact {
			what = fmt.Sprintf("loads artifact %q", strings.TrimPrefix(p.key, artifactPrefix))
			artifacts++
		} else {
			states++
		}
		fmt.Fprintf(&b, "\n  line %d: %s → %s", p.line, p.text, what)
	}
	b.WriteString("\n\nADK resolves every {…} in an instruction before the prompt is sent, so a run with no " +
		"such key fails with \"state key does not exist\". Doubling the braces does not escape it — {{x}} is " +
		"trimmed to the same key. Write literal text as <x> instead.")
	if states > 0 {
		b.WriteString(" If session state really is what you want, mark it optional as {x?}, so an unset key " +
			"renders as nothing instead of ending the run.")
	}
	if artifacts > 0 {
		b.WriteString(" The optional marker will not rescue an artifact: mast runs no artifact service, and " +
			"ADK fails on that before it looks at the marker, so {artifact.x?} fails exactly as {artifact.x} does.")
	}
	return fmt.Errorf("%s", b.String())
}

// resolvablePlaceholders returns the placeholders in body that ADK will
// resolve and can fail on, in the order they appear.
func resolvablePlaceholders(body string) []badPlaceholder {
	var out []badPlaceholder
	for _, loc := range placeholderRegex.FindAllStringIndex(body, -1) {
		match := body[loc[0]:loc[1]]
		key := strings.TrimSpace(strings.Trim(match, "{}"))
		optional := strings.HasSuffix(key, "?")
		key = strings.TrimSuffix(key, "?")

		// Order follows ADK's: artifacts are recognised before the
		// optional marker is consulted, and before the name is checked
		// for validity at all.
		artifact := strings.HasPrefix(key, artifactPrefix)
		switch {
		case artifact:
			// Fails with or without the marker, for want of a service.
		case optional:
			// An unset key renders as nothing. Nothing to warn about,
			// and the escape hatch this error recommends.
			continue
		case !isValidStateName(key):
			// ADK hands these back verbatim: JSON, jsonpath, prose.
			continue
		}
		out = append(out, badPlaceholder{
			text:     match,
			key:      key,
			artifact: artifact,
			line:     1 + strings.Count(body[:loc[0]], "\n"),
		})
	}
	return out
}

// isValidStateName mirrors ADK's: a bare identifier, or one of the
// three scope prefixes followed by an identifier.
func isValidStateName(name string) bool {
	prefix, rest, found := strings.Cut(name, ":")
	if !found {
		return isIdentifier(name)
	}
	if strings.Contains(rest, ":") {
		return false
	}
	for _, p := range statePrefixes {
		if prefix+":" == p {
			return isIdentifier(rest)
		}
	}
	return false
}

// isIdentifier mirrors ADK's, which mirrors Python's str.isidentifier.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && !unicode.IsLetter(r) && r != '_' {
			return false
		}
		if i > 0 && !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

func plural(n int, word string) string {
	if n == 1 {
		return "a " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func verb(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
