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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-steer/mast/pkg/mcp"
	"github.com/go-steer/mast/pkg/permissions"
)

// TestCatalogFileNameIsControlPlane is the lockstep test the permissions
// package's controlplane.go comment promises: it binds mcp.CatalogFileName
// to the permission gate's control-plane classification. A stdio catalog
// entry names a command mast executes, so a model must never be able to
// write the catalog even under yolo mode. pkg/mcp imports permissions
// (one-directional — permissions deliberately does not import mcp), so a
// rename of CatalogFileName that diverged from the gate's "mcp.json"
// literal would fail here rather than silently drop the executed catalog
// out of write-protection.
func TestCatalogFileNameIsControlPlane(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(root, ".agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(agents, mcp.CatalogFileName)
	if err := os.WriteFile(catalog, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	scope, err := permissions.NewPathScope(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Yolo mode auto-approves ordinary writes; a control-plane write must
	// still be refused with no prompter.
	g := permissions.New(permissions.Options{Mode: permissions.ModeYolo, Scope: scope})
	err = g.CheckFileWrite(context.Background(), "write_file", catalog)
	if err == nil {
		t.Fatalf("write to %s must be refused as control-plane even under yolo", mcp.CatalogFileName)
	}
	if !errors.Is(err, permissions.ErrControlPlaneWrite) {
		t.Errorf("error = %v, want ErrControlPlaneWrite", err)
	}
}
