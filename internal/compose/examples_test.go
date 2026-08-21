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

package compose

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
	"github.com/go-steer/mast/pkg/workload"
)

// exampleWorkloadsDir is the tree every shipped example bundle lives in.
const exampleWorkloadsDir = "../../examples/workloads"

// TestEveryExampleBundleStillBuilds loads and composes every bundle
// under examples/workloads, offline.
//
// The examples are documentation that executes, and the loaders they
// execute against keep gaining refusals: a tier that conflicts with a
// model (#149), an output_schema that will not parse, a roster whose
// read/write split does not hold (CheckCapabilitySplit), and
// tools.skills, which the specialist loader started rejecting outright
// at #211. Each of those is a good refusal and each of them can turn a
// bundle that shipped as an example into one that no longer boots,
// with the failure landing on an operator's first run rather than in
// CI. Only gke-triage was exercised before this test — by
// dev/ci/presubmits/e2e.sh and deploy/projection_test.go — which left
// bounded-triage and ns-audit as prose that nothing compiled.
//
// The directory is globbed rather than listed so a new example is
// covered by existing it, with no second edit to remember and no state
// in which a bundle is quietly uncovered. That framing is core-agent's
// (68ad89a, which found examples/parallel-spawn had been exiting 1 for
// some time while its README advertised "Exits 0"); the artifacts here
// are bundles rather than Go programs, so the gate is a build rather
// than a run.
func TestEveryExampleBundleStillBuilds(t *testing.T) {
	entries, err := os.ReadDir(exampleWorkloadsDir)
	if err != nil {
		t.Fatalf("read %s: %v", exampleWorkloadsDir, err)
	}
	var found int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found++
		t.Run(e.Name(), func(t *testing.T) {
			root := filepath.Join(exampleWorkloadsDir, e.Name())
			b, err := workload.Load(filepath.Join(root, "workload.yaml"))
			if err != nil {
				t.Fatalf("workload.Load: %v", err)
			}
			specs, err := specialists.LoadDir(filepath.Join(root, "specialists"))
			if err != nil {
				t.Fatalf("specialists.LoadDir: %v", err)
			}
			// "echo" is what makes this credential-free: offline fakes
			// collapse per-specialist model and tier overrides back to
			// the one fake, so a tiered roster composes here without a
			// provider client. A real model name would send tier
			// resolution to genai for an API key, which is exactly the
			// dependency that gets a gate skipped.
			a, _, err := BuildRoot(context.Background(), RootConfig{
				Bundle:    b,
				Specs:     specs,
				Model:     mastagent.NewEchoModel("echo"),
				ModelName: "echo",
			})
			if err != nil {
				t.Fatalf("BuildRoot: %v", err)
			}
			if a == nil || a.Name() == "" {
				t.Fatalf("BuildRoot returned an unnamed root agent")
			}
			t.Logf("%s: %d specialists, root %q", e.Name(), len(specs), a.Name())
		})
	}
	if found == 0 {
		t.Fatalf("no example bundles found under %s — the glob has lost its tree", exampleWorkloadsDir)
	}
}
