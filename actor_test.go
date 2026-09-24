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

package mast_test

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/mast"
	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/transcript"
)

// consumedBy gate-pauses a fresh session, resumes it with ResumeByToken
// under ctx, and returns what the pause record says consumed it. A gate
// pause is the shortest path to ConsumedBy: its resume runs no turn.
func consumedBy(t *testing.T, ctx context.Context) string {
	t.Helper()
	cfg := mast.Config{ModelName: "echo", Sessions: adksession.InMemoryService()}
	bundle, specs := triageBundle(false)
	res, err := mast.RunWorkload(context.Background(), cfg, bundle, specs, injectInput)
	if err != nil {
		t.Fatalf("RunWorkload: %v", err)
	}
	h, err := mast.Pause(context.Background(), cfg, res.SessionID,
		transcript.PauseSpec{Reason: transcript.ReasonMaintenanceWindow})
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := mast.ResumeByToken(ctx, cfg, bundle, specs, h.Token, nil); err != nil {
		t.Fatalf("ResumeByToken: %v", err)
	}
	// "mast" is the app name library sessions are stored under; it is
	// pinned to cmd/mast's so `mast sessions` reads an embedded DB.
	rec, err := transcript.NewStore(cfg.Sessions, "mast").FindToken(context.Background(), h.Token)
	if err != nil {
		t.Fatalf("FindToken: %v", err)
	}
	if rec.ConsumedAt.IsZero() {
		t.Fatal("the token is still live after ResumeByToken")
	}
	return rec.ConsumedBy
}

func TestResumeByTokenRecordsTheActorOnCtx(t *testing.T) {
	ctx := mast.WithActor(context.Background(), "alice@example.com")
	if got := consumedBy(t, ctx); got != "alice@example.com" {
		t.Errorf("ConsumedBy = %q, want the name WithActor put on ctx", got)
	}
}

func TestResumeByTokenWithoutAnActorNamesTheMechanism(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no actor":    context.Background(),
		"empty actor": mast.WithActor(context.Background(), ""),
	} {
		if got := consumedBy(t, ctx); got != "library ResumeByToken" {
			t.Errorf("%s: ConsumedBy = %q, want the mechanism, not a guessed human", name, got)
		}
	}
}

func TestResumeByTokenNoLongerReadsPkgAuth(t *testing.T) {
	// The break, pinned so it is a decision rather than an accident. The
	// root package used to read pkg/auth's Caller off ctx, which put an
	// unsupported package's context key inside the v1.0 promise. An
	// embedder still setting it now gets the mechanism, and the
	// CHANGELOG says so; WithActor is the replacement.
	ctx := auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"})
	if got := consumedBy(t, ctx); got != "library ResumeByToken" {
		t.Errorf("ConsumedBy = %q: the root package is reading pkg/auth's context key again", got)
	}
}

// promised is DESIGN.md's list of import paths v1.0 covers, as
// directories relative to this one.
var promised = []string{
	".",
	"pkg/agent",
	"pkg/transcript",
	"pkg/workload",
	"pkg/specialists",
	"pkg/budget",
}

func TestNoPromisedPackageImportsTheAuthPackages(t *testing.T) {
	// Caller identity is moving to go-steer/purser, and pkg/auth and
	// pkg/serverauth are both on DESIGN.md's unsupported list so they can
	// be deleted when it does. A promised package importing either would
	// quietly commit whatever it reaches — a type in a signature, or a
	// context key, which no signature shows — and turn that deletion into
	// a major.
	banned := map[string]bool{
		"github.com/go-steer/mast/pkg/auth":       true,
		"github.com/go-steer/mast/pkg/serverauth": true,
	}
	fset := token.NewFileSet()
	checked := 0
	for _, dir := range promised {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			checked++
			for _, spec := range f.Imports {
				imp, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatalf("%s: bad import %s: %v", path, spec.Path.Value, err)
				}
				if banned[imp] {
					t.Errorf("%s imports %s, which v1.0 does not promise and purser replaces", path, imp)
				}
			}
		}
	}
	if checked < len(promised) {
		t.Fatalf("checked %d files across %d promised packages; this test measured nothing", checked, len(promised))
	}
}
