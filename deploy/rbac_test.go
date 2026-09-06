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

package deploy

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	baseDir   = "base"
	targetDir = "remediation-target"

	daemonSA  = "mast-daemon"
	daemonNS  = "mast-triage"
	wifScript = "../scripts/setup-wif.sh"
	daemonSAM = "base/10-serviceaccount-daemon.yaml"

	// wifUserSuffix is the tail of the RBAC username the API server gives
	// the daemon's Workload Identity Federation principal. The head is
	// "serviceAccount:" plus the project ID, which the manifests carry as
	// a REPLACE_ME placeholder, so only the two ends are pinnable here.
	//
	// It is a *User*, not a ServiceAccount, and that is the whole point:
	// see TestBindingsNameTheMCPPathSubject.
	wifUserPrefix = "serviceAccount:"
	wifUserSuffix = ".svc.id.goog[" + daemonNS + "/" + daemonSA + "]"
)

// cluster-write IAM roles: bound to the KSA principal, any one of these
// authorizes every mutating call in every namespace over the MCP path,
// whatever the manifests in this directory say.
var clusterWriteIAM = []string{
	"roles/container.admin",
	"roles/container.developer",
	"roles/container.clusterAdmin",
	"roles/editor",
	"roles/owner",
}

// writeVerbs are the verbs that change the cluster. "*" is here because
// a wildcard grant includes all of them.
var writeVerbs = []string{"create", "update", "patch", "delete", "deletecollection", "*"}

// rbacDoc is the subset of an RBAC object these tests reason about.
type rbacDoc struct {
	file     string
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Subjects []rbacSubject `yaml:"subjects"`
}

type rbacSubject struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// TestDaemonHoldsNoClusterWideWrite is the RBAC mirror's load-bearing
// assertion (v0.3 W2.6): read is cluster-wide, change is not.
//
// It walks from the subject rather than from the file — every
// ClusterRoleBinding that names the daemon's ServiceAccount, whatever
// it is called and wherever it lives — because the way this boundary
// actually erodes is a new cluster-scoped grant added for one tool,
// not an edit to the file named "read". Widening
// 14-clusterrole-daemon-read.yaml with a single "patch" fails here.
//
// Secrets get their own check: a cluster-wide secret read on an agent
// that hands what it reads to a model is an exfiltration path, and it
// carries no write verb, so nothing else here would catch it.
func TestDaemonHoldsNoClusterWideWrite(t *testing.T) {
	docs := rbacDocs(t, baseDir, targetDir)
	roles := byName(docs, "ClusterRole")

	var bound int
	for _, b := range docs {
		if b.Kind != "ClusterRoleBinding" || !bindsDaemon(b) {
			continue
		}
		role, ok := roles[b.RoleRef.Name]
		if !ok {
			t.Errorf("%s binds the daemon to ClusterRole %q, which no manifest defines", b.file, b.RoleRef.Name)
			continue
		}
		bound++
		for _, r := range role.Rules {
			for _, v := range r.Verbs {
				if slices.Contains(writeVerbs, strings.ToLower(v)) {
					t.Errorf("%s grants the daemon %q on %v cluster-wide — write verbs belong in the namespaced Role (%s/)",
						role.file, v, r.Resources, targetDir)
				}
			}
			for _, res := range r.Resources {
				if res == "*" {
					t.Errorf("%s grants the daemon every resource cluster-wide", role.file)
				}
				if strings.HasPrefix(res, "secrets") {
					t.Errorf("%s grants the daemon %q cluster-wide — diagnosis never needs secret contents", role.file, res)
				}
			}
		}
	}
	if bound == 0 {
		t.Fatalf("no ClusterRoleBinding names ServiceAccount %s/%s — the daemon has no read grant at all", daemonNS, daemonSA)
	}
}

// TestDaemonWriteGrantIsNamespaced pins the other half: the write verbs
// exist, they are in a Role bound by a RoleBinding, and that pair is
// not rendered by deploy/base.
//
// The last clause is the structural one. deploy/base/kustomization.yaml
// sets `namespace: mast-triage` for everything it renders, so a write
// Role carried there would land in the daemon's own namespace — and an
// operator retargeting it at a real namespace would silently widen the
// grant for every deployment built from the base. Keeping the pair in
// its own kustomization makes each remediable namespace a separate,
// deliberate apply.
func TestDaemonWriteGrantIsNamespaced(t *testing.T) {
	if !workloadDeclaresAWrite(t) {
		t.Skip("the deployed workload declares no mutating tool, so no write grant is required")
	}

	docs := rbacDocs(t, targetDir)
	roles := byName(docs, "Role")

	var granted int
	for _, b := range docs {
		if b.Kind != "RoleBinding" || !bindsDaemon(b) {
			continue
		}
		if b.RoleRef.Kind != "Role" {
			t.Errorf("%s binds the daemon to a %s — a RoleBinding to a ClusterRole grants that ClusterRole's rules, so the split has to be in the role too",
				b.file, b.RoleRef.Kind)
			continue
		}
		role, ok := roles[b.RoleRef.Name]
		if !ok {
			t.Errorf("%s binds the daemon to Role %q, which no manifest in %s defines", b.file, b.RoleRef.Name, targetDir)
			continue
		}
		for _, r := range role.Rules {
			for _, v := range r.Verbs {
				if v == "*" {
					t.Errorf("%s grants every verb on %v — the write surface is enumerated, not wildcarded", role.file, r.Resources)
				}
				if slices.Contains(writeVerbs, strings.ToLower(v)) {
					granted++
				}
			}
			for _, res := range r.Resources {
				if res == "*" {
					t.Errorf("%s grants every resource in the namespace", role.file)
				}
				// Cluster-scoped kinds in a Role are inert, so naming
				// one means somebody expected it to work.
				if slices.Contains([]string{"nodes", "namespaces", "persistentvolumes", "storageclasses", "clusterroles", "clusterrolebindings"}, res) {
					t.Errorf("%s names cluster-scoped %q in a namespaced Role — it grants nothing and reads as if it does", role.file, res)
				}
				if strings.HasPrefix(res, "secrets") {
					t.Errorf("%s grants %q — remediation does not write secrets", role.file, res)
				}
			}
		}
	}
	if granted == 0 {
		t.Errorf("the workload declares mutating tools but no namespaced Role grants the daemon a write verb")
	}

	// The pair must be invisible to the base's namespace transformer.
	for _, res := range kustomizationResources(t, filepath.Join(baseDir, "kustomization.yaml")) {
		if strings.Contains(res, targetDir) {
			t.Errorf("deploy/%s/kustomization.yaml renders %q — the write grant would be pinned to %s", baseDir, res, daemonNS)
		}
	}
	if ns := kustomizationNamespace(t, filepath.Join(targetDir, "kustomization.yaml")); ns == "" {
		t.Errorf("deploy/%s/kustomization.yaml sets no namespace: — a Role with no namespace lands wherever the operator's kubectl context points", targetDir)
	}
}

// TestKustomizationsRenderEveryManifest catches the registration a new
// manifest is one step away from missing. kustomize renders only what
// `resources:` names; a ClusterRole added to the directory and left out
// of the list is a file that reviews as shipped and deploys as absent —
// which for an RBAC manifest means the grant silently doesn't exist,
// and for a *narrowing* one means the wide grant it replaces stays.
func TestKustomizationsRenderEveryManifest(t *testing.T) {
	for _, dir := range []string{baseDir, targetDir} {
		listed := map[string]bool{}
		for _, r := range kustomizationResources(t, filepath.Join(dir, "kustomization.yaml")) {
			listed[r] = true
		}
		for _, name := range manifestNames(t, dir) {
			if !listed[name] {
				t.Errorf("deploy/%s/%s is not in that directory's kustomization resources: — it is never applied", dir, name)
			}
			delete(listed, name)
		}
		for name := range listed {
			t.Errorf("deploy/%s/kustomization.yaml lists %q, which is not in the directory", dir, name)
		}
	}
}

// TestWifDefaultRoleIsDocumented couples the IAM binding to its
// disclosure.
//
// GKE authorizes a Kubernetes call if EITHER IAM or RBAC allows it, and
// mast reaches the cluster through the GKE MCP server as the KSA's WIF
// principal — so the project-level role setup-wif.sh binds decides
// whether the manifests above bound anything at all. It is bound by a
// shell script and described in a YAML comment, with nothing but this
// test between them. Change one and the other has to keep up.
func TestWifDefaultRoleIsDocumented(t *testing.T) {
	script := readFile(t, wifScript)
	sa := readFile(t, daemonSAM)

	scope, role := wifDefault(t, script)
	if !strings.Contains(sa, role) {
		t.Errorf("%s binds %s by default (WRITE_SCOPE=%s), which %s does not mention — the manifest's role list is what an operator reads", wifScript, role, scope, daemonSAM)
	}
	if !strings.Contains(script, "WRITE_SCOPE=namespaced") {
		t.Errorf("%s no longer documents the narrowing that makes the RBAC split load-bearing", wifScript)
	}
	// While the default is a cluster-write role, the manifest has to say
	// so: an operator who reads only the RBAC files would otherwise
	// conclude the daemon cannot change kube-system, and it can.
	if slices.Contains(clusterWriteIAM, role) && !strings.Contains(sa, "WARNING") {
		t.Errorf("%s binds cluster-write %s by default and %s carries no warning about it", wifScript, role, daemonSAM)
	}
}

// TestBindingsNameTheMCPPathSubject is the assertion #290 cost a live
// GKE cluster to learn.
//
// mast's tools do not talk to the API server as the pod's ServiceAccount.
// They go through the GKE MCP server, which reaches the cluster as the
// KSA's Workload Identity Federation principal, and the API server names
// that principal
//
//	serviceAccount:PROJECT.svc.id.goog[mast-triage/mast-daemon]
//
// as an RBAC **User**. GKE does not resolve it to the ServiceAccount
// subject. So a binding that names only `kind: ServiceAccount` grants the
// path mast actually uses exactly nothing — and the failure is silent in
// the direction that hurts twice over: with cluster-write IAM the write
// works anyway and the RBAC file looks load-bearing when it isn't; with
// the narrowed IAM the daemon is read-only everywhere and the operator
// has an agent that cannot remediate for reasons no manifest explains.
// Measured both ways on live GKE 2026-09-06.
//
// Every binding that names the daemon has to name both. The read
// ClusterRoleBinding included: without it the MCP path cannot even list
// pods.
func TestBindingsNameTheMCPPathSubject(t *testing.T) {
	docs := rbacDocs(t, baseDir, targetDir)

	var checked int
	for _, b := range docs {
		if b.Kind != "ClusterRoleBinding" && b.Kind != "RoleBinding" {
			continue
		}
		if !bindsDaemonSA(b) {
			continue
		}
		checked++
		if !bindsDaemonWifUser(b) {
			t.Errorf("%s binds the daemon's ServiceAccount but no `kind: User` named serviceAccount:<project>%s — it grants the in-cluster path only, and mast's tools do not use it",
				b.file, wifUserSuffix)
		}
	}
	if checked == 0 {
		t.Fatalf("no binding in deploy/%s or deploy/%s names ServiceAccount %s/%s", baseDir, targetDir, daemonNS, daemonSA)
	}

	// The placeholder has to survive to the operator. A subject with a
	// real project ID baked in is a subject that silently matches nobody
	// on every cluster but the one it was written for.
	for _, b := range docs {
		for _, s := range b.Subjects {
			if s.Kind != "User" || !strings.HasSuffix(s.Name, wifUserSuffix) {
				continue
			}
			if !strings.Contains(s.Name, "REPLACE_ME_PROJECT") {
				t.Errorf("%s names WIF subject %q with no REPLACE_ME_PROJECT placeholder — it is pinned to one project", b.file, s.Name)
			}
		}
	}
}

// wifDefault resolves setup-wif.sh's default WRITE_SCOPE to the IAM role
// that case arm binds. Reading the arm alone is not enough: which arm is
// the default is itself a decision that has changed (#290).
func wifDefault(t *testing.T, script string) (scope, role string) {
	t.Helper()

	_, rest, ok := strings.Cut(script, `WRITE_SCOPE="${WRITE_SCOPE:-`)
	if !ok {
		t.Fatalf("%s no longer sets a default WRITE_SCOPE", wifScript)
	}
	scope, _, ok = strings.Cut(rest, "}")
	if !ok {
		t.Fatalf("%s sets WRITE_SCOPE in a shape this test cannot read", wifScript)
	}
	role, ok = shellAssignment(script, scope+") CONTAINER_ROLE=")
	if !ok {
		t.Fatalf("%s defaults WRITE_SCOPE to %q, which no case arm binds a CONTAINER_ROLE for", wifScript, scope)
	}
	return scope, role
}

// bindsDaemon reports whether a binding names the daemon by either of
// the two usernames it reaches a cluster under. A grant added for one
// path is a grant, so the write-verb and secret checks above have to see
// both.
func bindsDaemon(b rbacDoc) bool {
	return bindsDaemonSA(b) || bindsDaemonWifUser(b)
}

// bindsDaemonSA reports whether a binding names the daemon's
// ServiceAccount — the in-cluster path. An empty subject namespace means
// the binding's own, which for a RoleBinding is the remediation target —
// not where the daemon lives — so it is not a match.
func bindsDaemonSA(b rbacDoc) bool {
	for _, s := range b.Subjects {
		if s.Kind == "ServiceAccount" && s.Name == daemonSA && s.Namespace == daemonNS {
			return true
		}
	}
	return false
}

// bindsDaemonWifUser reports whether a binding names the daemon's
// Workload Identity Federation principal — the GKE MCP path.
func bindsDaemonWifUser(b rbacDoc) bool {
	for _, s := range b.Subjects {
		if s.Kind == "User" && strings.HasPrefix(s.Name, wifUserPrefix) && strings.HasSuffix(s.Name, wifUserSuffix) {
			return true
		}
	}
	return false
}

func byName(docs []rbacDoc, kind string) map[string]rbacDoc {
	out := map[string]rbacDoc{}
	for _, d := range docs {
		if d.Kind == kind {
			out[d.Metadata.Name] = d
		}
	}
	return out
}

// rbacDocs decodes every YAML document in the named directories,
// excluding each directory's kustomization.
func rbacDocs(t *testing.T, dirs ...string) []rbacDoc {
	t.Helper()
	var out []rbacDoc
	for _, dir := range dirs {
		for _, name := range manifestNames(t, dir) {
			path := filepath.Join(dir, name)
			f, err := os.Open(path) // #nosec G304 -- test-local, repo-relative manifest path
			if err != nil {
				t.Fatalf("open %s: %v", path, err)
			}
			dec := yaml.NewDecoder(f)
			for {
				var d rbacDoc
				err := dec.Decode(&d)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("parse %s: %v", path, err)
				}
				d.file = path
				out = append(out, d)
			}
			f.Close()
		}
	}
	if len(out) == 0 {
		t.Fatalf("no manifests found under %v", dirs)
	}
	return out
}

// manifestNames lists a directory's YAML files, sorted, minus its
// kustomization.
func manifestNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" || e.Name() == "kustomization.yaml" {
			continue
		}
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

func kustomizationResources(t *testing.T, path string) []string {
	t.Helper()
	var k struct {
		Resources []string `yaml:"resources"`
	}
	unmarshalFile(t, path, &k)
	return k.Resources
}

func kustomizationNamespace(t *testing.T, path string) string {
	t.Helper()
	var k struct {
		Namespace string `yaml:"namespace"`
	}
	unmarshalFile(t, path, &k)
	return k.Namespace
}

// workloadDeclaresAWrite reports whether the deployed bundle classifies
// any tool as mutating — the reason a write grant has to exist at all.
func workloadDeclaresAWrite(t *testing.T) bool {
	t.Helper()
	var w struct {
		ToolCatalog struct {
			Tools []struct {
				Name     string `yaml:"name"`
				Mutating bool   `yaml:"mutating"`
			} `yaml:"tools"`
		} `yaml:"tool_catalog"`
	}
	unmarshalFile(t, filepath.Join(configDir, "workload.yaml"), &w)
	for _, tool := range w.ToolCatalog.Tools {
		if tool.Mutating {
			return true
		}
	}
	return false
}

// shellAssignment returns the quoted value assigned after prefix, e.g.
// `namespaced) CONTAINER_ROLE="roles/container.viewer"` -> the role.
func shellAssignment(script, prefix string) (string, bool) {
	_, rest, ok := strings.Cut(script, prefix)
	if !ok {
		return "", false
	}
	rest = strings.TrimPrefix(rest, `"`)
	value, _, ok := strings.Cut(rest, `"`)
	return value, ok
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test-local, repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
