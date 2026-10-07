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

package cli

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProjectInstruction(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Unset is off, and reads nothing — not even the working directory.
	if got, err := loadProjectInstruction(logger, ""); err != nil || got != "" {
		t.Fatalf("unset: got %q, %v; want nothing", got, err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Cluster: projects/p/locations/l/clusters/c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadProjectInstruction(logger, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(got, "projects/p/locations/l/clusters/c") {
		t.Errorf("got %q; want the AGENTS.md text", got)
	}

	// A directory with nothing in it was named to say something; saying
	// nothing in silence is refused.
	if _, err := loadProjectInstruction(logger, t.TempDir()); err == nil || !strings.Contains(err.Error(), "no AGENTS.md") {
		t.Errorf("empty dir: err = %v; want a refusal naming AGENTS.md", err)
	}
}
