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

package mcp_test

import (
	"context"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/mcp"
)

// TestNewGKEToolset_Compose exercises the construction path. If
// Application Default Credentials happen to be available on the host,
// the test asserts the toolset built cleanly; otherwise it asserts the
// error is well-formed (mentions ADC) so operators get a useful hint.
func TestNewGKEToolset_Compose(t *testing.T) {
	ctx := context.Background()
	ts, err := mcp.NewGKEToolset(ctx, mcp.GKEConfig{})
	if err != nil {
		// Expected on a bare sandbox — no ADC configured. Verify the
		// error message points the operator at the fix.
		msg := err.Error()
		if !strings.Contains(msg, "load Google default credentials") &&
			!strings.Contains(msg, "initial Google OAuth token fetch") {
			t.Fatalf("unexpected error shape: %v", err)
		}
		t.Logf("ADC not available (expected on bare sandbox): %v", err)
		return
	}
	if ts == nil {
		t.Fatal("NewGKEToolset returned nil toolset with nil error")
	}
	t.Log("ADC available; toolset constructed cleanly")
}
