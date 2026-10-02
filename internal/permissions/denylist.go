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

// Originally derived from go-steer/core-agent@83ec0713ade7a5c05d72ad280039f336f561414b

// Package permissions implements the central permission gate that
// decides whether each tool invocation may proceed.
//
// The gate consults, in order:
//  1. Bash denylist (built-in patterns; best-effort defense-in-depth,
//     see below) for bash calls.
//  2. Path scope check for file tools.
//  3. Config denylist patterns.
//  4. Config allowlist patterns.
//  5. Mode-specific resolution: ask → prompt user; allow → deny;
//     yolo → approve.
//
// The interactive prompt path is implemented by the host (TUI / CLI REPL);
// see prompter.go for the Prompter interface.
//
// # Port status: partly wired (corrected 2026-08-17)
//
// Two entry points are live. internal/compose/writegate.go builds a
// Gate and pkg/approval drives it: CheckMutatingToolCall on every call
// the workload's mutation predicate classifies as mutating, and
// RecordMutationVerdict on the operator's answer. That path runs the
// plan-first pre-check (planExemptTools in gate.go is therefore live
// policy, not a dormant table), the config deny policy, and ModePlan.
//
// The rest is not. Nothing calls CheckBash, CheckPath or CheckGeneric,
// because mast registers no bash tool and no built-in file tools —
// its mutations arrive over MCP. Nothing populates Settings from a
// config surface either (see settings.go). So the bash denylist
// documented below and the path-scope check are compiled and tested
// but unreached; a reader looking for what protects mast today should
// look at the write gate, not at this file.
//
// This note previously said the whole package was unwired and that
// "nothing in mast is protected by these checks". That stopped being
// true when the write gate landed in v0.3 (W2.1–W2.3) and was not
// updated; the 2026-08-17 upstream triage found it
// (docs/sibling-sync.md).
//
// # Wiring-time design inputs (recorded 2026-07-27, from upstream
// go-steer/core-agent#385; resolved upstream the same day as #465):
// (1) plan-first exempting network egress — resolved. fetch_url is no
// longer exempt in either repo; the read-only skill namespace stays
// exempt, and gate.go says why. (2) acceptEdits auto-allowing
// out-of-scope filesystem writes — upstream documented the blast
// radius louder rather than narrowing the mode. mast reached a
// stronger answer by a different route: CheckMutatingToolCall refuses
// to let ModeAcceptEdits (or ModeYolo, or ModeAllow) short-circuit a
// mutation approval at all, so the mode cannot auto-allow a write here
// regardless of scope. Nothing further is owed at wiring time.
//
// # The bash denylist is defense-in-depth, NOT a security boundary
//
// The built-in bash denylist below (regexDenylist + the rm -rf target
// list) runs before allow/deny/mode resolution and is not overridable
// by config — but do NOT mistake "not overridable" for "a guarantee."
// It is a small, pattern-based refusal list for the handful of shell
// forms most likely to brick a machine by accident (or on the first,
// laziest attempt by a prompt-injected model). It is trivially evaded
// by anyone who wants to, and it is the ONLY bash protection left once
// a command reaches yolo mode or a session/verb grant. Known bypass
// classes it does not and cannot catch:
//
//   - Quoting / whitespace tricks: rm -rf "$HOME", rm -r${IFS}-f ~.
//   - Variable / expansion indirection: X=/ ; rm -rf "$X", eval, base64
//     | sh, $(printf ...).
//   - Staging: curl … -o /tmp/x; sh /tmp/x (each command looks benign).
//   - Uncovered targets: rm -rf /etc, rm -rf ~/important, mv over a
//     device, chmod on a non-root path — the target list is a
//     hard-coded handful, not "everything dangerous."
//
// A regex denylist over an unbounded shell grammar is a losing game by
// construction; treating it as a boundary is a mistake. The hardened
// posture is allowlist-based execution: run in `allow` mode (or `ask`
// with a Prompter) and grant only the specific commands you intend,
// using the safecmd-guarded prefix allow rules (see policy.go /
// builtin_allow.go). The denylist is the seatbelt, not the brakes.
package permissions

import (
	"regexp"
	"strings"
)

// regexRule is a denylist entry: a regexp matched verbatim against the
// trimmed bash command, paired with a short user-facing reason.
type regexRule struct {
	pat    *regexp.Regexp
	reason string
}

var regexDenylist = []regexRule{
	{regexp.MustCompile(`\bdd\s+if=\S+\s+of=/dev/`), "refuses to write directly to a device file"},
	{regexp.MustCompile(`\bmkfs(\.[a-z0-9]+)?\b`), "refuses to format a filesystem"},
	{regexp.MustCompile(`\bshred\s+`), "refuses to securely overwrite files"},
	{regexp.MustCompile(`\bwipefs\s+`), "refuses to wipe filesystem signatures"},
	{regexp.MustCompile(`\bchmod\s+-R\s+[0-7]{3,4}\s+/(\s|$)`), "refuses to chmod the entire filesystem root"},
	{regexp.MustCompile(`\bchown\s+-R\s+\S+\s+/(\s|$)`), "refuses to chown the entire filesystem root"},
	{regexp.MustCompile(`\b(curl|wget)\s+\S[^|]*\|\s*(sh|bash|zsh|ash|dash)\b`), "refuses to execute a downloaded script directly"},
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), "refuses to execute a fork bomb"},
}

// dangerousRmTargets lists path arguments that, combined with both -r
// and -f flags on rm, trigger a refusal. Compared after normalization.
var dangerousRmTargets = map[string]struct{}{
	"/":        {},
	"/*":       {},
	"~":        {},
	"~/":       {},
	"~/*":      {},
	"$HOME":    {},
	"$HOME/":   {},
	"${HOME}":  {},
	"${HOME}/": {},
	"/.":       {},
}

// IsBashDenied reports whether command matches any built-in denylist
// pattern. The reason is a short, user-facing string suitable for
// surfacing in a prompt or stderr.
//
// This is best-effort defense-in-depth, not a security boundary: a
// false return means "no built-in pattern matched," NOT "this command
// is safe." See the package doc for the bypass classes it cannot
// catch and why allowlist-based execution is the hardened posture.
func IsBashDenied(command string) (denied bool, reason string) {
	if r := checkDangerousRm(command); r != "" {
		return true, r
	}
	for _, r := range regexDenylist {
		if r.pat.MatchString(command) {
			return true, r.reason
		}
	}
	return false, ""
}

// checkDangerousRm returns a non-empty reason string if command is
// `rm`-with-recursive-and-force pointed at a destructive target. The
// flag parsing intentionally accepts any combination/order (-rf, -fr,
// -Rfv, --recursive --force, etc.).
func checkDangerousRm(command string) string {
	tokens := strings.Fields(strings.TrimSpace(command))
	if len(tokens) < 3 || tokens[0] != "rm" {
		return ""
	}
	hasR, hasF := false, false
	var positional []string
	for _, t := range tokens[1:] {
		switch {
		case t == "--recursive":
			hasR = true
		case t == "--force":
			hasF = true
		case strings.HasPrefix(t, "--"):
			// other long flags (e.g. --no-preserve-root) — ignored
		case strings.HasPrefix(t, "-"):
			for _, c := range t[1:] {
				switch c {
				case 'r', 'R':
					hasR = true
				case 'f', 'F':
					hasF = true
				}
			}
		default:
			positional = append(positional, t)
		}
	}
	if !hasR || !hasF {
		return ""
	}
	for _, p := range positional {
		if _, bad := dangerousRmTargets[p]; bad {
			return "refuses to recursively delete the filesystem root or $HOME"
		}
	}
	return ""
}
