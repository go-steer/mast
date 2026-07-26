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
	"io"
	"strings"
	"testing"
)

// TestSweep smoke-tests the starter offline: plan → parallel workers →
// join → summarize, with deterministic output across runs.
func TestSweep(t *testing.T) {
	root, err := buildRoot()
	if err != nil {
		t.Fatalf("buildRoot: %v", err)
	}
	r, err := newRunner(root)
	if err != nil {
		t.Fatalf("newRunner: %v", err)
	}

	out, err := runSweep(context.Background(), r, "test-1", "alpha, beta, gamma", io.Discard)
	if err != nil {
		t.Fatalf("runSweep: %v", err)
	}
	summary, ok := out.(string)
	if !ok {
		t.Fatalf("terminal output type = %T, want string", out)
	}
	if !strings.Contains(summary, "3 services checked") {
		t.Errorf("summary missing header: %q", summary)
	}
	for _, svc := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(summary, svc) {
			t.Errorf("summary missing service %q: %q", svc, summary)
		}
	}

	// Determinism: a second run over the same fleet must produce the
	// same summary (fresh session so no history bleeds in).
	again, err := runSweep(context.Background(), r, "test-2", "alpha, beta, gamma", io.Discard)
	if err != nil {
		t.Fatalf("runSweep (second): %v", err)
	}
	if again != out {
		t.Errorf("non-deterministic summary:\nfirst:  %v\nsecond: %v", out, again)
	}
}
