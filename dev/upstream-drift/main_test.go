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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttributionRe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		line     string
		wantSHA  string
		wantPath string
	}{
		{"go plain", "// Originally derived from go-steer/core-agent@83ec0713ade7a5c05d72ad280039f336f561414b", "83ec0713ade7a5c05d72ad280039f336f561414b", ""},
		{"go short", "// Originally derived from go-steer/core-agent@c5efbb9e", "c5efbb9e", ""},
		{"go with path", "// Originally derived from go-steer/core-agent@c5efbb9e:pkg/mcp/lifecycle.go", "c5efbb9e", "pkg/mcp/lifecycle.go"},
		{"shell comment", "# Originally derived from go-steer/core-agent@25d8531c", "25d8531c", ""},
		{"indented yaml", "  # Originally derived from go-steer/core-agent@25d8531c", "25d8531c", ""},

		// Must NOT match. dev/tools/.golangci.yml documents the
		// convention in prose with a literal placeholder; treating the
		// lint config as a ported file would put a phantom row in every
		// report.
		{"placeholder", `      # "Originally derived from go-steer/core-agent@<SHA>" line (the`, "", ""},
		{"prose mention", "The trailer reads Originally derived from go-steer/core-agent@83ec071 and so on", "", ""},
		{"too short", "// Originally derived from go-steer/core-agent@abc", "", ""},
		{"wrong upstream", "// Originally derived from go-steer/core-tui@83ec0713", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := attributionRe.FindStringSubmatch(tc.line)
			if tc.wantSHA == "" {
				if m != nil {
					t.Fatalf("expected no match, got %q", m[0])
				}
				return
			}
			if m == nil {
				t.Fatalf("expected a match for %q", tc.line)
			}
			if m[1] != tc.wantSHA {
				t.Errorf("sha = %q, want %q", m[1], tc.wantSHA)
			}
			if m[2] != tc.wantPath {
				t.Errorf("path = %q, want %q", m[2], tc.wantPath)
			}
		})
	}
}

func TestMapPath(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		// Unmapped packages sit at the same path upstream.
		{"pkg/attach/server.go", "pkg/attach/server.go"},
		{"pkg/pricing/builtin.go", "pkg/pricing/builtin.go"},

		// The fork renamed pkg/models to pkg/providers.
		{"pkg/providers/gemini/gemini.go", "pkg/models/gemini/gemini.go"},
		{"pkg/providers/anthropic/llm.go", "pkg/models/anthropic/llm.go"},

		// Longest match wins: vertexcache is not under upstream's
		// pkg/models at all, so the more specific rewrite must beat the
		// pkg/providers one it is nested inside.
		{"pkg/providers/vertexcache/manager.go", "internal/vertexcache/manager.go"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := mapPath(tc.in); got != tc.want {
				t.Errorf("mapPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestScanFindsTrailersAndSkipsWorktrees(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	write(t, root, "pkg/attach/server.go", "// Copyright\n\n// Originally derived from go-steer/core-agent@aaaaaaa\n\npackage attach\n")
	write(t, root, "pkg/providers/gemini/gemini.go", "// Originally derived from go-steer/core-agent@bbbbbbb\npackage gemini\n")
	write(t, root, "pkg/mcp/auth.go", "// Originally derived from go-steer/core-agent@ccccccc:pkg/mcp/lifecycle.go\npackage mcp\n")
	// Not ported.
	write(t, root, "pkg/agent/agent.go", "package agent\n")
	// Documents the convention; not a ported file.
	write(t, root, "docs/fork-design.md", "// Originally derived from go-steer/core-agent@ddddddd\n")
	// A nested agent worktree. Both repos keep these under .claude, and
	// walking into one double-counts every ported file in the tree.
	write(t, root, ".claude/worktrees/x/pkg/attach/server.go", "// Originally derived from go-steer/core-agent@aaaaaaa\npackage attach\n")

	got, err := scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 3 {
		var paths []string
		for _, a := range got {
			paths = append(paths, a.Path)
		}
		t.Fatalf("scan found %d files (%v), want 3", len(got), paths)
	}

	byPath := map[string]attribution{}
	for _, a := range got {
		byPath[a.Path] = a
	}
	if a := byPath["pkg/providers/gemini/gemini.go"]; a.UpstreamPath != "pkg/models/gemini/gemini.go" {
		t.Errorf("gemini upstream path = %q, want the pkg/models rewrite", a.UpstreamPath)
	}
	// An explicit :path in the trailer beats every inferred mapping.
	if a := byPath["pkg/mcp/auth.go"]; a.UpstreamPath != "pkg/mcp/lifecycle.go" {
		t.Errorf("mcp upstream path = %q, want the explicit trailer path", a.UpstreamPath)
	}
}

// TestCompare exercises the three outcomes against a real git history:
// commits found, upstream rename, and a SHA the clone doesn't have.
func TestCompare(t *testing.T) {
	t.Parallel()
	up := newRepo(t)

	write(t, up, "pkg/pricing/pricing.go", "package pricing // v1\n")
	gitDo(t, up, "add", "-A")
	commitAs(t, up, "feat(pricing): initial")
	portedAt := revParse(t, up, "HEAD")

	write(t, up, "pkg/pricing/pricing.go", "package pricing // v2\n")
	gitDo(t, up, "add", "-A")
	commitAs(t, up, "fix(pricing): cap the response body")

	write(t, up, "pkg/pricing/pricing.go", "package pricing // v3\n")
	gitDo(t, up, "add", "-A")
	commitAs(t, up, "feat(pricing): cache-write rates")

	// An unrelated file must not inflate the count.
	write(t, up, "pkg/attach/server.go", "package attach\n")
	gitDo(t, up, "add", "-A")
	commitAs(t, up, "feat(attach): unrelated")

	t.Run("counts only commits touching the file", func(t *testing.T) {
		res := compare(up, "HEAD", attribution{
			Path: "pkg/pricing/pricing.go", UpstreamPath: "pkg/pricing/pricing.go", SHA: portedAt,
		})
		if res.Status != statusOK {
			t.Fatalf("status = %q, want ok", res.Status)
		}
		if len(res.Commits) != 2 {
			t.Fatalf("commits = %d, want 2: %+v", len(res.Commits), res.Commits)
		}
		if !strings.Contains(res.Commits[0].Subject, "cache-write") {
			t.Errorf("newest commit first expected, got %q", res.Commits[0].Subject)
		}
	})

	t.Run("a file with no upstream change reports clean", func(t *testing.T) {
		at := revParse(t, up, "HEAD")
		res := compare(up, "HEAD", attribution{
			Path: "pkg/pricing/pricing.go", UpstreamPath: "pkg/pricing/pricing.go", SHA: at,
		})
		if res.Status != statusOK || len(res.Commits) != 0 {
			t.Fatalf("status=%q commits=%d, want ok/0", res.Status, len(res.Commits))
		}
	})

	// The false-negative this tool exists to avoid: upstream renames the
	// file, `git log -- <old path>` goes quiet, and a naive reader
	// concludes the port is current.
	t.Run("upstream rename is flagged, not counted as clean", func(t *testing.T) {
		res := compare(up, "HEAD", attribution{
			Path: "pkg/pricing/pricing.go", UpstreamPath: "pkg/pricing/gone.go", SHA: portedAt,
		})
		if res.Status != statusMissingPath {
			t.Fatalf("status = %q, want %q", res.Status, statusMissingPath)
		}
		if len(res.Commits) != 0 {
			t.Errorf("a missing path must not report commits, got %d", len(res.Commits))
		}
	})

	t.Run("a SHA absent from the clone is flagged", func(t *testing.T) {
		res := compare(up, "HEAD", attribution{
			Path: "pkg/pricing/pricing.go", UpstreamPath: "pkg/pricing/pricing.go",
			SHA: "0123456789abcdef0123456789abcdef01234567",
		})
		if res.Status != statusUnknownSHA {
			t.Fatalf("status = %q, want %q", res.Status, statusUnknownSHA)
		}
	})
}

func TestAggregateDedupesCommitsWithinAPackage(t *testing.T) {
	t.Parallel()
	// One upstream commit touching three files in a package is one
	// commit for a human to read, not three.
	shared := commit{SHA: "aaa1111", Subject: "refactor(attach): rename the broadcaster"}
	results := []fileResult{
		{attribution: attribution{Path: "pkg/attach/a.go", SHA: "1111111"}, Status: statusOK, Commits: []commit{shared}},
		{attribution: attribution{Path: "pkg/attach/b.go", SHA: "1111111"}, Status: statusOK, Commits: []commit{shared, {SHA: "bbb2222", Subject: "fix(attach): b only"}}},
		{attribution: attribution{Path: "pkg/attach/c.go", SHA: "1111111"}, Status: statusOK, Commits: []commit{shared}},
		{attribution: attribution{Path: "pkg/pricing/p.go", SHA: "2222222"}, Status: statusMissingPath},
	}

	got := aggregate(results)
	if len(got) != 2 {
		t.Fatalf("packages = %d, want 2", len(got))
	}
	// Sorted by commit count descending, so attach leads.
	if got[0].Dir != "pkg/attach" {
		t.Fatalf("first package = %q, want pkg/attach", got[0].Dir)
	}
	if len(got[0].Commits) != 2 {
		t.Errorf("attach commits = %d, want 2 (deduped)", len(got[0].Commits))
	}
	if got[0].Files != 3 {
		t.Errorf("attach files = %d, want 3", got[0].Files)
	}
	if len(got[1].Problems[statusMissingPath]) != 1 {
		t.Errorf("pricing should carry one missing-path problem, got %+v", got[1].Problems)
	}
}

// --- helpers ---

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a throwaway git repo under the test's temp dir — house
// rule #5: scratch state never lands in $HOME.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitDo(t, dir, "init", "-b", "main")
	return dir
}

func gitDo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// commitAs passes identity per-command so the test never depends on —
// or writes — global git config.
func commitAs(t *testing.T, dir, message string) {
	t.Helper()
	gitDo(t, dir,
		"-c", "user.email=test@example.com",
		"-c", "user.name=Test",
		"commit", "-m", message)
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := gitOut(dir, "rev-parse", ref)
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return out
}
