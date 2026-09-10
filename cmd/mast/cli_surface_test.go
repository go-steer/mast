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

package main

import (
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// updateCLISurface rewrites the golden instead of comparing against it.
// The other goldens in this repo (pkg/attach's agent card) are
// hand-maintained; this one is forty-odd rows across ten flag sets, and
// a hand-edit that gets a row wrong writes a false contract — which is
// worse than no contract, because the next reader trusts it.
var updateCLISurface = flag.Bool("update", false, "rewrite testdata/cli-surface.txt from the current flag sets")

const cliSurfaceGolden = "testdata/cli-surface.txt"

const cliSurfaceHeader = `# mast's CLI surface: flag names, types, defaults, subcommand verbs, and
# exit codes. This is a frozen contract as of v1.0 — see DESIGN.md, "The
# v1.0 stability promise". The binary is a first-class consumer shape
# alongside the library, and its callers are shell scripts, systemd
# units and Kubernetes manifests whose compiler cannot see this repo, so
# nothing else in the tree fails when a flag is renamed.
#
# A diff here is an intentional CLI change. Adding a flag or a verb is a
# minor; renaming or removing one, or changing a type, a default, or
# what an exit code means, is a major.
#
# Deliberately absent: usage strings. Help text is prose and is not part
# of the promise; pinning it would make every wording fix a contract
# diff and teach readers to update this file without looking.
#
# Regenerate: go test ./cmd/mast/ -run TestCLISurface -update
`

// TestCLISurface pins the command-line contract.
func TestCLISurface(t *testing.T) {
	got := renderCLISurface()

	if *updateCLISurface {
		if err := os.WriteFile(cliSurfaceGolden, []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", cliSurfaceGolden, err)
		}
		t.Logf("wrote %s", cliSurfaceGolden)
		return
	}

	want, err := os.ReadFile(cliSurfaceGolden)
	if err != nil {
		t.Fatalf("read %s: %v", cliSurfaceGolden, err)
	}
	if got != string(want) {
		t.Fatalf("the CLI surface drifted from %s.\n"+
			"If the change is intentional, regenerate with -update and say in the PR body whether it is a minor (additions only) or a major (rename, removal, type or default change).\n"+
			"--- got ---\n%s\n--- want ---\n%s", cliSurfaceGolden, got, want)
	}
}

// TestSessionsVerbsAreReal checks the verb list the golden is rendered
// from against the two things that could make it a lie: a verb nobody
// implements, and a verb the usage text never mentions.
func TestSessionsVerbsAreReal(t *testing.T) {
	t.Parallel()
	for _, verb := range sessionsVerbs {
		if _, _, err := sessionsFlagSet(verb); err != nil {
			t.Errorf("sessionsVerbs names %q, which sessionsFlagSet does not implement: %v", verb, err)
		}
		if !strings.Contains(sessionsUsage, verb) {
			t.Errorf("sessionsVerbs names %q, which the usage text never mentions", verb)
		}
	}
	if _, _, err := sessionsFlagSet("not-a-verb"); err == nil {
		t.Error("sessionsFlagSet accepted an undeclared verb; the verb list is not gating anything")
	}
}

// renderCLISurface walks every flag set the binary installs and renders
// them in a stable order.
func renderCLISurface() string {
	var b strings.Builder
	b.WriteString(cliSurfaceHeader)

	b.WriteString("\nexit codes\n")
	for _, c := range []struct {
		code int
		name string
		what string
	}{
		{exitOK, "ok", "the work completed"},
		{exitFailure, "failure", "the work was attempted and failed"},
		{exitUsage, "usage", "the invocation was rejected before any work started"},
		{exitDrainExpired, "drain-expired", "serve mode only: the shutdown drain expired with sessions still interrupted"},
	} {
		fmt.Fprintf(&b, "  %d  %-13s %s\n", c.code, c.name, c.what)
	}

	b.WriteString("\nmast [flags] [prompt]\n")
	writeFlags(&b, flagSetOf(registerRunFlags))

	fmt.Fprintf(&b, "\nmast sessions <verb>\n  verbs: %s\n", strings.Join(sortedVerbs(), " "))
	for _, verb := range sortedVerbs() {
		fs, _, err := sessionsFlagSet(verb)
		if err != nil {
			fmt.Fprintf(&b, "\n  mast sessions %s\n    ERROR: %v\n", verb, err)
			continue
		}
		fmt.Fprintf(&b, "\n  mast sessions %s\n", verb)
		writeFlagsIndent(&b, fs, "    ")
	}

	b.WriteString("\nmast stop\n")
	stopFS, _, _, _ := stopFlagSet()
	writeFlags(&b, stopFS)

	return b.String()
}

// sortedVerbs returns sessionsVerbs alphabetically. The declared order
// is the help text's; the golden's is stable under a help reshuffle.
func sortedVerbs() []string {
	out := append([]string(nil), sessionsVerbs...)
	sort.Strings(out)
	return out
}

// flagSetOf runs a registration function against a throwaway FlagSet,
// so enumerating the surface never touches flag.CommandLine.
func flagSetOf(register func(*flag.FlagSet) *runFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("mast", flag.ContinueOnError)
	register(fs)
	return fs
}

func writeFlags(b *strings.Builder, fs *flag.FlagSet) { writeFlagsIndent(b, fs, "  ") }

func writeFlagsIndent(b *strings.Builder, fs *flag.FlagSet, indent string) {
	// VisitAll is already lexical by name.
	fs.VisitAll(func(f *flag.Flag) {
		fmt.Fprintf(b, "%s--%-20s %-9s %q\n", indent, f.Name, flagKind(f), f.DefValue)
	})
}

// flagKind names a flag's value type. The stdlib keeps the concrete
// types unexported (*flag.stringValue and friends), so the name is
// derived from the type rather than asserted against it — a new kind
// renders its own name and shows up in the diff rather than panicking.
func flagKind(f *flag.Flag) string {
	name := reflect.TypeOf(f.Value).String()
	name = strings.TrimPrefix(name, "*")
	name = strings.TrimPrefix(name, "flag.")
	return strings.TrimSuffix(name, "Value")
}
