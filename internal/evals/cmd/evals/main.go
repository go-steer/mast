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

// Command evals runs the v0.3 parity eval suite (docs/v0.3-plan.md W0.4).
//
// Normally invoked through scripts/evals.sh, which is what
// dev/ci/presubmits/evals.sh and the ci.yml evals job run. Direct use:
//
//	go run ./internal/evals/cmd/evals              # deterministic tier
//	go run ./internal/evals/cmd/evals --format=json
//
// Exit codes mirror the harness's own three-valued outcome, because the
// distinction matters to whoever reads the CI log:
//
//	0  the suite gates green
//	1  a scenario missed its declared outcome, or a metric is a constant
//	2  the harness could not run — bad fixture, unknown or unbuilt tier
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-steer/mast/internal/evals/harness"
)

func main() {
	var (
		tier   = flag.String("tier", harness.TierDeterministic, "which tier to run: deterministic (free, gating) or judge (metered, W0.5)")
		root   = flag.String("root", "", "repository root; defaults to the nearest ancestor with a go.mod")
		format = flag.String("format", "text", "output format: text or json")
	)
	flag.Parse()

	err := run(*tier, *root, *format)
	switch {
	case err == nil:
	case errors.Is(err, errFailed):
		// The report already said what is red and why; repeating it on
		// stderr would only bury it.
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "evals: %v\n", err)
		os.Exit(2)
	}
}

// errFailed marks the "suite ran and something is red" case, so main can
// tell it apart from "the harness could not run".
var errFailed = errors.New("suite failed")

func run(tier, root, format string) error {
	if root == "" {
		r, err := findRoot()
		if err != nil {
			return err
		}
		root = r
	}
	if format != "text" && format != "json" {
		return fmt.Errorf("unknown format %q (want text or json)", format)
	}

	sum, err := harness.Run(context.Background(), harness.Config{Root: root, Tier: tier})
	if err != nil {
		return err
	}

	switch format {
	case "json":
		if err := sum.WriteJSON(os.Stdout); err != nil {
			return err
		}
	default:
		sum.WriteText(os.Stdout)
	}
	if !sum.OK() {
		return errFailed
	}
	return nil
}

// findRoot walks up from the working directory for the module root, so
// the command works from anywhere in the tree rather than only from the
// directory the fixtures happen to be relative to.
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod in any parent of the working directory; pass --root")
		}
		dir = parent
	}
}
