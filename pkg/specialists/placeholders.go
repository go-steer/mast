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

// Braces in a prompt are literal (#464, narrowing #272).
//
// This file used to refuse a specialist whose body contained `{project}`,
// because ADK resolved every `{...}` in an instruction before sending it
// and a missing key killed the run. That was true, and the refusal was
// the best available fix while mast still handed its prompts to
// llmagent.Config.Instruction — a template field.
//
// It no longer does. pkg/agent passes every prompt through
// InstructionProvider, which ADK forwards unchanged, so a brace is a
// brace on all three surfaces: a specialist body, a bundle's coordinator
// instruction, and the planner's rendered prompt. Nothing scans them and
// nothing can fail on them. See pkg/agent/instruction.go.
//
// # What is left to refuse, and why it is only this
//
// One syntax changed meaning *silently*, and it is the only thing this
// check still reports: the optional marker.
//
//	{project?}        was: inject session-state key "project", or nothing
//	{app:project?}    was: the same, app-scoped
//	{artifact.x?}     was: load an artifact (and fail — mast runs none)
//
// An author wrote those to ask for injection, and injection is gone.
// Left alone they would render as the literal text `{project?}`, which
// is not what the file says and not what the run would do. mast's
// standing position on a silent downgrade is to fail the load and name
// the file (#302, and the `.tmpl` removal in #349), so that is what
// happens here.
//
// Everything else now loads, including the shapes this file used to
// reject. `{project}`, `{app:web}` and `{artifact.report}` are ordinary
// text today; refusing them would be mast restricting prose it has
// promised to pass through verbatim, for a runtime hazard that no
// longer exists. A prompt full of Kubernetes manifests, jsonpath and
// shell variables — `${MAST_HOME}/bin/mast` — is simply fine, which was
// always the point.
//
// # Why the refusal is permanent rather than a deprecation window
//
// Same reasoning as the `.tmpl` constant next door: every bundle written
// before this change may carry an optional marker, and there is no date
// after which telling its author that it stopped injecting becomes wrong.
//
// # What guards the other direction
//
// This file no longer tracks ADK's resolution rule, so it no longer
// pins itself to ADK's copy of it — the test that did was removed with
// the coupling it guarded. What must not regress is the *cause*: a
// future constructor reaching for Config.Instruction would bring the
// templating back, silently, for every prompt. That is guarded by
// TestNoShippedCodePassesAPromptThroughADKsTemplateField in pkg/agent,
// which is an AST check over the whole module rather than a rule
// restated here.

package specialists

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// placeholderRegex is ADK's, as it stood when mast used the template
// field. It is frozen at that shape deliberately: its job is to find
// what an author wrote back when the marker meant something, so an ADK
// bump that changes the rule must *not* change this.
var placeholderRegex = regexp.MustCompile(`{+[^{}]*}+`)

// statePrefixes are the three qualified state scopes ADK recognised.
// Anything else before a colon made the whole name invalid.
var statePrefixes = []string{"app:", "user:", "temp:"}

// artifactPrefix marked an artifact load rather than a state lookup.
const artifactPrefix = "artifact."

// staleInjection is one optional-marked placeholder: text an author
// wrote to ask for injection, which now renders as itself.
type staleInjection struct {
	// text is the placeholder exactly as written, braces included, so
	// the author can find it by searching for it.
	text string
	// key is what ADK would have resolved, after the braces and the
	// marker are trimmed.
	key string
	// artifact distinguishes the two, which get different advice: a
	// state lookup had a value to lose, an artifact load never did.
	artifact bool
	line     int
}

// checkPlaceholders reports every optional-marked placeholder in a
// template body, as one error naming all of them.
//
// All of them rather than the first: an author who fixes one and
// restarts to find the next has learned the rule the slowest possible
// way, and a prompt with braces in it usually has several.
func checkPlaceholders(path, body string) error {
	stale := staleInjections(body)
	if len(stale) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "specialists: %q: %s in the template body no longer %s anything — mast sends instructions verbatim",
		path, plural(len(stale), "placeholder"), verb(len(stale)))
	states, artifacts := 0, 0
	for _, p := range stale {
		what := fmt.Sprintf("asked for session-state key %q", p.key)
		if p.artifact {
			what = fmt.Sprintf("asked for artifact %q", strings.TrimPrefix(p.key, artifactPrefix))
			artifacts++
		} else {
			states++
		}
		fmt.Fprintf(&b, "\n  line %d: %s → %s", p.line, p.text, what)
	}
	b.WriteString("\n\nmast no longer substitutes anything into a prompt: the body reaches the model exactly as " +
		"written, braces included. Drop the marker and write the text you want the model to read.")
	if states > 0 {
		b.WriteString(" Session state is no longer reachable from a prompt at all; pass what the specialist " +
			"needs to know in the request, or have the caller write it into the instruction.")
	}
	if artifacts > 0 {
		b.WriteString(" The artifact form never loaded anything — mast runs no artifact service — so there is " +
			"nothing to replace it with.")
	}
	return fmt.Errorf("%s", b.String())
}

// staleInjections returns the optional-marked placeholders in body, in
// the order they appear.
func staleInjections(body string) []staleInjection {
	var out []staleInjection
	for _, loc := range placeholderRegex.FindAllStringIndex(body, -1) {
		match := body[loc[0]:loc[1]]
		key := strings.TrimSpace(strings.Trim(match, "{}"))
		if !strings.HasSuffix(key, "?") {
			// Never resolved, or resolved into an error nobody could
			// have depended on. Literal text now, and left alone.
			continue
		}
		key = strings.TrimSuffix(key, "?")

		artifact := strings.HasPrefix(key, artifactPrefix)
		if !artifact && !isValidStateName(key) {
			// A trailing "?" does not make prose into a lookup: "{who
			// knows?}" was literal before and is literal now.
			continue
		}
		out = append(out, staleInjection{
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
		return "injects"
	}
	return "inject"
}
