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

// Command upstream-drift reports how far mast's ported files have
// fallen behind the go-steer/core-agent commits they were copied from.
//
// # Why this exists rather than a diff
//
// Every ported file carries an attribution trailer naming the upstream
// commit it was copied at:
//
//	// Originally derived from go-steer/core-agent@<sha>[:<upstream-path>]
//
// The obvious detector — diff the file against upstream — is useless
// here: 100% of ported files already differ. They differ by the
// attribution line itself, by ~/.mast vs ~/.core-agent paths, by
// mast-flavored comments, and (increasingly) by real product
// divergence. A diff-based report says "180 of 180 files differ" every
// week and means nothing.
//
// The question worth answering is the other one: *has upstream moved
// this file since we copied it?* That is a git-log query against the
// attributed SHA, and it is what this tool runs. It reports what
// changed upstream, not how the two copies differ today — a file mast
// deliberately rewrote still shows its upstream commits, because
// "upstream fixed a bug in code we rewrote" is exactly the case a
// human needs to look at.
//
// # Why it never fails the build
//
// Drift is normal and expected. Some of it is code mast will re-port
// wholesale when P1.3's gate opens (see docs/fork-design.md); some is
// in packages that have genuinely forked and will never sync again.
// A red build on a condition nobody can clear teaches people to ignore
// red builds. The signal goes to a job summary and a tracking issue;
// the exit status stays 0 unless the tool itself could not run.
//
// A non-zero exit therefore means one thing only: the report could not
// be produced (no clone, unreadable repo, unknown SHA). That
// distinction matters — a silent "0 commits of drift" caused by a
// broken clone is strictly worse than no report at all, so every way
// of failing to *look* is reported as a status on the row rather than
// counted as clean.
//
// Usage:
//
//	go run ./dev/upstream-drift --upstream ../core-agent
//	go run ./dev/upstream-drift --upstream ../core-agent --ref origin/main
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// attributionRe matches the port trailer. The upstream path suffix is
// optional and wins over every inferred mapping when present — it is
// the escape hatch for files that moved between the two trees
// (pkg/mcp/auth.go already uses it, having been split out of
// upstream's pkg/mcp/lifecycle.go).
//
// The SHA class is deliberately [0-9a-f]{7,40} rather than a wildcard:
// dev/tools/.golangci.yml documents this very convention in prose,
// with a literal "<SHA>" placeholder, and must not be mistaken for a
// ported file.
var attributionRe = regexp.MustCompile(`^\s*(?://|#)\s*Originally derived from go-steer/core-agent@([0-9a-f]{7,40})(?::(\S+))?\s*$`)

// scanExtensions are the file types that carry the trailer. Markdown
// is excluded on purpose: docs/fork-design.md *describes* the
// convention, and a design doc quoting a real SHA would otherwise
// register as a ported file.
var scanExtensions = map[string]bool{
	".go":   true,
	".sh":   true,
	".yml":  true,
	".yaml": true,
}

// skipDirs are pruned during the walk.
//
// `.claude` matters more than it looks: both repos keep agent worktrees
// under .claude/worktrees, so a naive walk finds every ported file once
// per worktree and reports drift numbers several times too large.
//
// `.upstream` is where the workflow checks the core-agent clone out.
// actions/checkout refuses a path outside the workspace, so the thing
// being compared against necessarily sits inside the tree being
// scanned; without this it would be scanned as if it were mast.
var skipDirs = map[string]bool{
	".git":         true,
	".claude":      true,
	".upstream":    true,
	"vendor":       true,
	"node_modules": true,
}

// dirRewrites map a mast directory onto its upstream counterpart where
// the fork renamed it. Longest match wins. Anything not listed here is
// assumed to sit at the same path upstream, which holds for the large
// majority (pkg/attach, pkg/permissions, pkg/pricing, ...).
//
// Keep this table small. A file that needs a one-off mapping should
// carry the `@<sha>:<path>` form in its own trailer instead, so the
// fact travels with the file rather than living in a lookup here.
var dirRewrites = map[string]string{
	"pkg/providers/vertexcache": "internal/vertexcache",
	"pkg/providers":             "pkg/models",
}

type attribution struct {
	// Path is repo-relative, slash-separated, in mast.
	Path string
	// UpstreamPath is where the same file lives in core-agent.
	UpstreamPath string
	// SHA is the upstream commit the file was copied at.
	SHA string
}

type commit struct {
	SHA     string
	Subject string
}

// status values for a scanned file.
const (
	statusOK = "ok"
	// statusMissingPath means the mapped upstream path does not exist
	// at the compared ref. Almost always an upstream rename, and the
	// reason it is called out loudly rather than counted as clean:
	// `git log -- <path-that-moved>` returns zero commits, which would
	// otherwise read as "fully in sync" for a file that has in fact
	// been restructured out from under us.
	statusMissingPath = "missing-path"
	// statusUnknownSHA means the attributed commit is not in the
	// clone — a shallow checkout, or an upstream force-push.
	statusUnknownSHA = "unknown-sha"
)

type fileResult struct {
	attribution
	Status  string
	Commits []commit
}

type packageResult struct {
	Dir      string
	Files    int
	SHAs     []string
	Commits  []commit // deduped across the package's files
	Problems map[string][]string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "upstream-drift: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		upstream = flag.String("upstream", "", "path to a go-steer/core-agent clone (full history required)")
		ref      = flag.String("ref", "origin/main", "upstream ref to compare against")
		root     = flag.String("root", ".", "root of the mast checkout to scan")
		maxList  = flag.Int("max-commits", 15, "per-package commit subjects to list before eliding")
	)
	flag.Parse()

	if *upstream == "" {
		return fmt.Errorf("--upstream is required (a core-agent clone; `git clone https://github.com/go-steer/core-agent`)")
	}
	if _, err := os.Stat(filepath.Join(*upstream, ".git")); err != nil {
		return fmt.Errorf("--upstream %q is not a git checkout: %w", *upstream, err)
	}

	head, err := gitOut(*upstream, "rev-parse", "--short", *ref)
	if err != nil {
		return fmt.Errorf("resolving %s in %s: %w", *ref, *upstream, err)
	}
	headDate, err := gitOut(*upstream, "show", "-s", "--format=%cs", *ref)
	if err != nil {
		return fmt.Errorf("dating %s: %w", *ref, err)
	}

	attrs, err := scan(*root)
	if err != nil {
		return err
	}
	if len(attrs) == 0 {
		return fmt.Errorf("no attributed files found under %q — wrong --root?", *root)
	}

	results := make([]fileResult, 0, len(attrs))
	for _, a := range attrs {
		results = append(results, compare(*upstream, *ref, a))
	}

	report(os.Stdout, results, *ref, head, headDate, *maxList)
	return nil
}

// scan walks root and returns every file carrying a port trailer.
func scan(root string) ([]attribution, error) {
	var out []attribution
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !scanExtensions[filepath.Ext(d.Name())] {
			return nil
		}
		sha, upstreamPath, found, err := readTrailer(path)
		if err != nil || !found {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if upstreamPath == "" {
			upstreamPath = mapPath(rel)
		}
		out = append(out, attribution{Path: rel, UpstreamPath: upstreamPath, SHA: sha})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// readTrailer looks for the attribution line in a file's header
// region. The trailer is by convention the comment group directly
// below the Apache header (dev/tools/.golangci.yml explains why it
// cannot live inside it), so a bounded read is enough and keeps the
// walk from paging in whole 2k-line sources.
func readTrailer(path string) (sha, upstreamPath string, found bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for line := 0; line < 40 && sc.Scan(); line++ {
		if m := attributionRe.FindStringSubmatch(sc.Text()); m != nil {
			return m[1], m[2], true, nil
		}
	}
	// A scan error on a binary or over-long line is not fatal: the file
	// simply isn't an attributed source file.
	if scanErr := sc.Err(); scanErr != nil {
		return "", "", false, nil
	}
	return "", "", false, nil
}

// mapPath translates a mast path to its upstream counterpart using the
// longest matching directory rewrite.
func mapPath(rel string) string {
	best := ""
	for from := range dirRewrites {
		if (rel == from || strings.HasPrefix(rel, from+"/")) && len(from) > len(best) {
			best = from
		}
	}
	if best == "" {
		return rel
	}
	return dirRewrites[best] + strings.TrimPrefix(rel, best)
}

// compare asks upstream what has landed on this file since it was
// ported.
func compare(upstream, ref string, a attribution) fileResult {
	res := fileResult{attribution: a, Status: statusOK}

	// Is the attributed commit even in this clone? Checked first so a
	// shallow checkout reports as such rather than as a clean file.
	if _, err := gitOut(upstream, "cat-file", "-e", a.SHA+"^{commit}"); err != nil {
		res.Status = statusUnknownSHA
		return res
	}
	// Does the path still exist upstream? An upstream rename makes the
	// log query below return nothing, which would read as "in sync".
	if _, err := gitOut(upstream, "cat-file", "-e", ref+":"+a.UpstreamPath); err != nil {
		res.Status = statusMissingPath
		return res
	}

	out, err := gitOut(upstream, "log", "--format=%h\x1f%s", a.SHA+".."+ref, "--", a.UpstreamPath)
	if err != nil {
		res.Status = statusMissingPath
		return res
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x1f", 2)
		if len(parts) != 2 {
			continue
		}
		res.Commits = append(res.Commits, commit{SHA: parts[0], Subject: parts[1]})
	}
	return res
}

// aggregate folds per-file results into per-package rows, deduping
// commits: one upstream commit touching twenty pkg/attach files is one
// commit to review, not twenty.
func aggregate(results []fileResult) []packageResult {
	byDir := map[string]*packageResult{}
	seen := map[string]map[string]bool{}

	for _, r := range results {
		dir := filepath.ToSlash(filepath.Dir(r.Path))
		pr := byDir[dir]
		if pr == nil {
			pr = &packageResult{Dir: dir, Problems: map[string][]string{}}
			byDir[dir] = pr
			seen[dir] = map[string]bool{}
		}
		pr.Files++
		if !contains(pr.SHAs, r.SHA) {
			pr.SHAs = append(pr.SHAs, r.SHA)
		}
		if r.Status != statusOK {
			pr.Problems[r.Status] = append(pr.Problems[r.Status], r.Path)
			continue
		}
		for _, c := range r.Commits {
			if seen[dir][c.SHA] {
				continue
			}
			seen[dir][c.SHA] = true
			pr.Commits = append(pr.Commits, c)
		}
	}

	out := make([]packageResult, 0, len(byDir))
	for _, pr := range byDir {
		sort.Strings(pr.SHAs)
		out = append(out, *pr)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Commits) != len(out[j].Commits) {
			return len(out[i].Commits) > len(out[j].Commits)
		}
		return out[i].Dir < out[j].Dir
	})
	return out
}

func report(w *os.File, results []fileResult, ref, head, headDate string, maxList int) {
	pkgs := aggregate(results)

	totalFiles, driftFiles, problems := 0, 0, 0
	allCommits := map[string]bool{}
	for _, r := range results {
		totalFiles++
		if r.Status != statusOK {
			problems++
			continue
		}
		if len(r.Commits) > 0 {
			driftFiles++
		}
		for _, c := range r.Commits {
			allCommits[c.SHA] = true
		}
	}

	fmt.Fprintf(w, "# Upstream drift vs go-steer/core-agent\n\n")
	fmt.Fprintf(w, "Compared against `%s` @ `%s` (%s).\n\n", ref, head, headDate)
	fmt.Fprintf(w, "**%d** upstream commits have landed on **%d** of mast's **%d** ported files since they were copied.\n\n",
		len(allCommits), driftFiles, totalFiles)
	if problems > 0 {
		fmt.Fprintf(w, "**%d** files could not be compared — see _Needs attention_ below.\n\n", problems)
	}

	fmt.Fprintf(w, "| Package | Files | Ported at | Upstream commits since |\n")
	fmt.Fprintf(w, "|---|---:|---|---:|\n")
	for _, p := range pkgs {
		note := ""
		if len(p.Problems) > 0 {
			note = " ⚠"
		}
		fmt.Fprintf(w, "| `%s`%s | %d | %s | %d |\n", p.Dir, note, p.Files, strings.Join(shorten(p.SHAs), ", "), len(p.Commits))
	}
	fmt.Fprintln(w)

	// Needs-attention section first: a file that could not be compared
	// is a louder signal than a file with twenty commits of drift,
	// because the twenty are at least visible.
	var attention []packageResult
	for _, p := range pkgs {
		if len(p.Problems) > 0 {
			attention = append(attention, p)
		}
	}
	if len(attention) > 0 {
		fmt.Fprintf(w, "## Needs attention\n\n")
		fmt.Fprintf(w, "These files could not be compared, so their rows above undercount. ")
		fmt.Fprintf(w, "`missing-path` almost always means an upstream rename — fix it by adding the ")
		fmt.Fprintf(w, "`@<sha>:<upstream-path>` form to the file's own trailer.\n\n")
		for _, p := range attention {
			for status, files := range p.Problems {
				for _, f := range files {
					fmt.Fprintf(w, "- `%s` — **%s**\n", f, status)
				}
			}
		}
		fmt.Fprintln(w)
	}

	for _, p := range pkgs {
		if len(p.Commits) == 0 {
			continue
		}
		fmt.Fprintf(w, "## `%s` — %d commits\n\n", p.Dir, len(p.Commits))
		for i, c := range p.Commits {
			if i == maxList {
				fmt.Fprintf(w, "- _… and %d more_\n", len(p.Commits)-maxList)
				break
			}
			fmt.Fprintf(w, "- `%s` %s\n", c.SHA, c.Subject)
		}
		fmt.Fprintln(w)
	}

	// Machine-readable trailer. An HTML comment so it renders as
	// nothing in a job summary or an issue body, while staying greppable
	// by the workflow that decides whether to open one.
	fmt.Fprintf(w, "<!-- drift commits=%d files=%d packages=%d problems=%d -->\n",
		len(allCommits), driftFiles, len(pkgs), problems)
}

func shorten(shas []string) []string {
	out := make([]string, 0, len(shas))
	for _, s := range shas {
		if len(s) > 7 {
			s = s[:7]
		}
		out = append(out, "`"+s+"`")
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
