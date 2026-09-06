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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// The identity of the configuration this daemon is running (#289).
//
// The failure this exists for has three ingredients and needs all
// three. The deployment's ConfigMap generator sets
// `disableNameSuffixHash: true`, so editing the workload ConfigMap does
// not change its name and does not roll the pod. Nothing reloads. And
// until this file, nothing said which configuration was in effect — no
// path, no hash, no mtime — so an operator who applied an edit and saw
// no change had nothing to grep for. Any one of the three is
// survivable; together they make "my change had no effect" the most
// expensive shape of support question there is.
//
// Two things are recorded here, and they answer different halves.
//
// The **identity**, logged once at startup, says what was loaded: the
// root, the bundle, a digest over the exact files the loaders read, and
// how many of them there were. That turns the diagnosis into a
// comparison the operator can make from outside — the running digest
// against a digest of what they applied.
//
// The **drift check** says when that stopped being true. mast's own
// deployment mounts the ConfigMap as a whole volume rather than through
// `subPath`, which is the case where the kubelet *does* update the
// files under a running pod. So the content on disk changes, the parse
// in memory does not, and the two disagree silently for as long as the
// pod lives. Detecting that is not reloading it: mast keeps serving
// what it loaded, and says so, because what a live reload should do
// with an in-flight turn, a parked approval or an armed cadence is a
// design question and this is a diagnosis.
type configIdentity struct {
	// Root is the directory the configuration was resolved from — the
	// workload directory in path mode, the config root in name mode.
	Root string

	// Bundle is the path of the workload.yaml that was parsed.
	Bundle string

	// Files are the artifacts the loaders actually read, sorted by
	// Name. Not a directory listing: a file nothing read cannot change
	// what the daemon is doing, and a file read from outside Root
	// (an `output_schema:` reached by a relative path, say) has to be
	// covered even though a walk of Root would miss it.
	Files []configFile

	// Digest is `sha256:` + the first 16 hex digits of a hash over
	// every file's name and content — the same truncation the decision
	// export uses for an approver. It is what an operator compares.
	Digest string

	// Bytes is the total size of Files, and Newest the most recent
	// modification time among them. Both are for the human reading the
	// startup line: a size that halved and an mtime from last week are
	// each recognisable in a way a hash is not.
	Bytes  int64
	Newest time.Time
}

// configFile is one artifact, hashed.
type configFile struct {
	// Name is the path relative to Root, or the path as loaded when it
	// lies outside Root. Relative on purpose: the digest is then a
	// function of the configuration's content and layout rather than of
	// where it happens to be mounted, so the digest an operator
	// computes from a checkout matches the one the pod logs.
	Name string
	Path string

	// Sum is the full hex sha256 of the content, or empty when the file
	// could not be read — in which case Err carries why. An unreadable
	// file is a change like any other and moves the digest, because a
	// projection that lost a key is exactly the silent downgrade this
	// is watching for.
	Sum string
	Err string

	Size    int64
	ModTime time.Time
}

// configWatchInterval is how often the daemon re-hashes what it loaded.
// A minute is well inside the kubelet's own ConfigMap sync period and
// costs a few dozen small reads, and nothing downstream is timed off
// it — the check only ever logs.
const configWatchInterval = time.Minute

// configPaths lists the files a loaded workload was built from. The
// bundle, every specialist template, every output schema one of them
// resolved, and the MCP catalog — the last one by construction rather
// than by record, because the catalog is read into a value that does
// not keep its path.
func configPaths(bundle *workload.Bundle, specs []specialists.Spec, catalogPath string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if bundle != nil {
		add(bundle.Filename)
	}
	for _, s := range specs {
		add(s.Filename)
		add(s.OutputSchemaPath)
	}
	// Not every workload declares an MCP surface, so a missing catalog
	// is normal and is skipped rather than recorded as unreadable.
	if catalogPath != "" {
		if _, err := os.Stat(catalogPath); err == nil {
			add(catalogPath)
		}
	}
	sort.Strings(out)
	return out
}

// identifyConfig hashes the given files and folds them into one digest.
func identifyConfig(root, bundlePath string, paths []string) configIdentity {
	id := configIdentity{Root: root, Bundle: bundlePath}
	for _, p := range paths {
		f := configFile{Name: relativeTo(root, p), Path: p}
		data, err := os.ReadFile(p)
		if err != nil {
			f.Err = err.Error()
		} else {
			sum := sha256.Sum256(data)
			f.Sum = hex.EncodeToString(sum[:])
			f.Size = int64(len(data))
			id.Bytes += f.Size
			if fi, statErr := os.Stat(p); statErr == nil {
				f.ModTime = fi.ModTime()
				if f.ModTime.After(id.Newest) {
					id.Newest = f.ModTime
				}
			}
		}
		id.Files = append(id.Files, f)
	}
	sort.Slice(id.Files, func(i, j int) bool { return id.Files[i].Name < id.Files[j].Name })

	// Name and content only. Size is derivable from the content, and
	// mtime deliberately is not in here: a ConfigMap remount rewrites
	// every mtime whether or not a byte changed, and a digest that
	// moved for that reason would cry wolf on the one signal this
	// exists to make trustworthy.
	h := sha256.New()
	for _, f := range id.Files {
		if f.Sum != "" {
			fmt.Fprintf(h, "%s\x00%s\n", f.Name, f.Sum)
			continue
		}
		fmt.Fprintf(h, "%s\x00unreadable\n", f.Name)
	}
	id.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
	return id
}

// relativeTo renders p against root, falling back to p itself when it
// does not live under root.
func relativeTo(root, p string) string {
	if root == "" {
		return p
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return filepath.ToSlash(rel)
}

// log writes the startup line. One line rather than one per file: the
// per-file sums are what the digest is for, and a daemon that printed
// seventeen hashes at boot would train its reader to skip the block
// that matters.
func (id configIdentity) log(logger *slog.Logger) {
	if logger == nil || len(id.Files) == 0 {
		return
	}
	attrs := []any{
		"root", id.Root,
		"bundle", id.Bundle,
		"digest", id.Digest,
		"files", len(id.Files),
		"bytes", id.Bytes,
	}
	if !id.Newest.IsZero() {
		attrs = append(attrs, "newest_mtime", id.Newest.UTC().Format(time.RFC3339))
	}
	if unread := id.unreadable(); len(unread) > 0 {
		attrs = append(attrs, "unreadable", strings.Join(unread, ","))
	}
	logger.Info("workload config identity", attrs...)
}

func (id configIdentity) unreadable() []string {
	var out []string
	for _, f := range id.Files {
		if f.Sum == "" {
			out = append(out, f.Name)
		}
	}
	return out
}

func (id configIdentity) paths() []string {
	out := make([]string, 0, len(id.Files))
	for _, f := range id.Files {
		out = append(out, f.Path)
	}
	return out
}

// changedFrom names the files whose content differs between the two
// identities, including ones that appeared, vanished or stopped being
// readable. Names rather than a count: "specialists/CrashLoopBackOff.tmpl
// changed" is the whole diagnosis, and a count is a second question.
func (id configIdentity) changedFrom(prev configIdentity) []string {
	was := make(map[string]string, len(prev.Files))
	for _, f := range prev.Files {
		was[f.Name] = f.Sum
	}
	var out []string
	for _, f := range id.Files {
		old, ok := was[f.Name]
		delete(was, f.Name)
		switch {
		case !ok:
			out = append(out, f.Name+" (new)")
		case f.Sum == "" && old != "":
			out = append(out, f.Name+" (unreadable)")
		case f.Sum != old:
			out = append(out, f.Name)
		}
	}
	for name := range was {
		out = append(out, name+" (gone)")
	}
	sort.Strings(out)
	return out
}

// watchConfig re-hashes the loaded configuration on a cadence and says
// when what is on disk stops matching what is running.
//
// Edge-triggered on the on-disk digest, so a single edit produces a
// single WARN and a second edit produces a second one. Reverting the
// change logs that the two agree again, because a stale warning about a
// condition that has been fixed is worse than none: the next operator
// reads it as current.
//
// It never reloads and never refuses a turn. The daemon is serving what
// it parsed at startup, which is a coherent configuration; the file on
// disk may be half-written, and applying it under an in-flight turn is
// the question this deliberately does not answer.
func watchConfig(ctx context.Context, logger *slog.Logger, loaded configIdentity, interval time.Duration) {
	if logger == nil || len(loaded.Files) == 0 || interval <= 0 {
		return
	}
	paths := loaded.paths()
	reported := loaded.Digest
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cur := identifyConfig(loaded.Root, loaded.Bundle, paths)
		if cur.Digest == reported {
			continue
		}
		reported = cur.Digest
		if cur.Digest == loaded.Digest {
			logger.Info("workload config on disk matches the running config again",
				"digest", loaded.Digest)
			continue
		}
		logger.Warn("WORKLOAD CONFIG ON DISK NO LONGER MATCHES THE RUNNING CONFIG — the edit has not taken effect",
			"running_digest", loaded.Digest,
			"on_disk_digest", cur.Digest,
			"changed", strings.Join(cur.changedFrom(loaded), ", "),
			"remedy", "mast does not reload configuration; restart the daemon (kubectl rollout restart) to pick this up")
	}
}
