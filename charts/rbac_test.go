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

package charts

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	wifScript = "../scripts/setup-wif.sh"
	// daemonSAT is the template an operator reads the daemon's IAM
	// requirements out of. TestWifDefaultRoleIsDocumented holds it to
	// what the script actually binds.
	daemonSAT = "mast/templates/serviceaccount-daemon.yaml"
	// workloadFile is the chart's copy of the deployed bundle.
	workloadFile = "mast/files/workload.yaml"
)

// cluster-write IAM roles: bound to the KSA principal, any one of these
// authorizes every mutating call in every namespace over the MCP path,
// whatever the manifests in this chart say.
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

// TestDaemonHoldsNoClusterWideWrite is the RBAC mirror's load-bearing
// assertion: read is cluster-wide, change is not.
//
// It walks from the subject rather than from the template — every
// ClusterRoleBinding that names the daemon, whatever it is called —
// because the way this boundary actually erodes is a new cluster-scoped
// grant added for one tool, not an edit to the template named "read".
//
// Secrets get their own check: a cluster-wide secret read on an agent
// that hands what it reads to a model is an exfiltration path, and it
// carries no write verb, so nothing else here would catch it.
func TestDaemonHoldsNoClusterWideWrite(t *testing.T) {
	docs := remediatingRender(t)
	roles := byName(docs, "ClusterRole")

	var bound int
	for _, b := range docs {
		if b.Kind != "ClusterRoleBinding" || !bindsDaemon(b) {
			continue
		}
		role, ok := roles[b.RoleRef.Name]
		if !ok {
			t.Errorf("%s binds the daemon to ClusterRole %q, which the chart does not render", b.id(), b.RoleRef.Name)
			continue
		}
		bound++
		for _, r := range role.Rules {
			for _, v := range r.Verbs {
				if slices.Contains(writeVerbs, strings.ToLower(v)) {
					t.Errorf("%s grants the daemon %q on %v cluster-wide — write verbs belong in the per-namespace Role",
						role.id(), v, r.Resources)
				}
			}
			for _, res := range r.Resources {
				if res == "*" {
					t.Errorf("%s grants the daemon every resource cluster-wide", role.id())
				}
				if strings.HasPrefix(res, "secrets") {
					t.Errorf("%s grants the daemon %q cluster-wide — diagnosis never needs secret contents", role.id(), res)
				}
			}
		}
	}
	if bound == 0 {
		t.Fatalf("no ClusterRoleBinding names the daemon — it has no read grant at all")
	}
}

// TestDefaultInstallGrantsNoWrite pins the default the kustomize base
// could not express.
//
// remediationNamespaces is empty out of the box, and an install that
// names no namespace must render no write verb anywhere — not a Role
// in the daemon's own namespace, not a disabled one, nothing. Under the
// base this was a separate directory an operator had to remember not to
// apply; here it is the absence of a list entry, so it is worth an
// assertion that the absence really means nothing is granted.
func TestDefaultInstallGrantsNoWrite(t *testing.T) {
	for _, d := range defaultRender(t) {
		if d.Kind != "Role" && d.Kind != "ClusterRole" {
			continue
		}
		for _, r := range d.Rules {
			for _, v := range r.Verbs {
				if slices.Contains(writeVerbs, strings.ToLower(v)) {
					t.Errorf("a default install renders %s granting %q on %v — it should grant no write anywhere",
						d.id(), v, r.Resources)
				}
			}
		}
	}
}

// TestDaemonWriteGrantIsNamespaced pins the other half: the write verbs
// exist when an operator asks for them, they are in a Role bound by a
// RoleBinding, and both land in the namespace that was asked for rather
// than in the daemon's own.
//
// The last clause is the structural one. The daemon's namespace is the
// one place a write grant is both useless and dangerous — useless
// because nothing mast remediates lives there, dangerous because it
// would let the agent rewrite its own Deployment and ConfigMap.
func TestDaemonWriteGrantIsNamespaced(t *testing.T) {
	if !workloadDeclaresAWrite(t) {
		t.Skip("the deployed workload declares no mutating tool, so no write grant is required")
	}

	docs := remediatingRender(t)
	roles := byName(docs, "Role")
	want := map[string]bool{"team-a": false, "team-b": false}

	var granted int
	for _, b := range docs {
		if b.Kind != "RoleBinding" || !bindsDaemon(b) {
			continue
		}
		if b.RoleRef.Kind != "Role" {
			t.Errorf("%s binds the daemon to a %s — a RoleBinding to a ClusterRole grants that ClusterRole's rules, so the split has to be in the role too",
				b.id(), b.RoleRef.Kind)
			continue
		}
		if b.Metadata.Namespace == releaseNS {
			t.Errorf("%s puts a write grant in the daemon's own namespace", b.id())
		}
		if _, ok := want[b.Metadata.Namespace]; !ok {
			t.Errorf("%s grants writes in %q, which is not a namespace the values named", b.id(), b.Metadata.Namespace)
			continue
		}
		want[b.Metadata.Namespace] = true

		// A RoleBinding only resolves a Role in its own namespace, so
		// the lookup is namespace-qualified: a Role rendered into the
		// wrong namespace is a binding that grants nothing.
		role, ok := roles[qualified(b.Metadata.Namespace, b.RoleRef.Name)]
		if !ok {
			t.Errorf("%s binds Role %q, which the chart does not render in %q",
				b.id(), b.RoleRef.Name, b.Metadata.Namespace)
			continue
		}
		for _, r := range role.Rules {
			for _, v := range r.Verbs {
				if v == "*" {
					t.Errorf("%s grants every verb on %v — the write surface is enumerated, not wildcarded", role.id(), r.Resources)
				}
				if slices.Contains(writeVerbs, strings.ToLower(v)) {
					granted++
				}
			}
			for _, res := range r.Resources {
				if res == "*" {
					t.Errorf("%s grants every resource in the namespace", role.id())
				}
				// Cluster-scoped kinds in a Role are inert, so naming
				// one means somebody expected it to work.
				if slices.Contains([]string{"nodes", "namespaces", "persistentvolumes", "storageclasses", "clusterroles", "clusterrolebindings"}, res) {
					t.Errorf("%s names cluster-scoped %q in a namespaced Role — it grants nothing and reads as if it does", role.id(), res)
				}
				if strings.HasPrefix(res, "secrets") {
					t.Errorf("%s grants %q — remediation does not write secrets", role.id(), res)
				}
			}
		}
	}
	if granted == 0 {
		t.Errorf("the workload declares mutating tools but no namespaced Role grants the daemon a write verb")
	}
	for ns, saw := range want {
		if !saw {
			t.Errorf("remediationNamespaces named %q and no RoleBinding landed there", ns)
		}
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
//	serviceAccount:PROJECT.svc.id.goog[NAMESPACE/mast-daemon]
//
// as an RBAC **User**. GKE does not resolve it to the ServiceAccount
// subject. So a binding that names only `kind: ServiceAccount` grants the
// path mast actually uses exactly nothing — and the failure is silent in
// the direction that hurts twice over: with cluster-write IAM the write
// works anyway and the RBAC reads as load-bearing when it isn't; with
// the narrowed IAM the daemon is read-only everywhere and the operator
// has an agent that cannot remediate for reasons no manifest explains.
// Measured both ways on live GKE 2026-09-06.
//
// Every binding that names the daemon has to name both. The read
// ClusterRoleBinding included: without it the MCP path cannot even list
// pods.
func TestBindingsNameTheMCPPathSubject(t *testing.T) {
	var checked int
	for _, b := range remediatingRender(t) {
		if b.Kind != "ClusterRoleBinding" && b.Kind != "RoleBinding" {
			continue
		}
		if !bindsDaemonSA(b) {
			continue
		}
		checked++
		if !bindsDaemonWifUser(b) {
			t.Errorf("%s binds the daemon's ServiceAccount but no `kind: User` named %q — it grants the in-cluster path only, and mast's tools do not use it",
				b.id(), wifUser)
		}
	}
	if checked == 0 {
		t.Fatalf("no rendered binding names ServiceAccount %s/%s", releaseNS, daemonSA)
	}
}

// TestChartRefusesToRenderWithoutAProject is the reason this is a chart
// and not a kustomization, expressed as a test.
//
// The project ID is substituted into an RBAC subject name. Leave it out
// of a kustomize overlay and the placeholder renders into a
// ClusterRoleBinding the API server happily accepts and that binds
// nobody — the #290 failure, with no symptom until a tool call comes
// back Forbidden mid-incident. Here the omission has to stop the
// install instead, and the message has to say which value is missing.
func TestChartRefusesToRenderWithoutAProject(t *testing.T) {
	out, err := helmTemplate(t)
	if err == nil {
		t.Fatalf("the chart rendered with no gcp.projectID — a subject with an empty project binds nobody:\n%s", out)
	}
	if !strings.Contains(string(out), "gcp.projectID is required") {
		t.Errorf("rendering without a project failed, but not with a message naming the value:\n%s", out)
	}
}

// TestWifDefaultRoleIsDocumented couples the IAM binding to its
// disclosure.
//
// GKE authorizes a Kubernetes call if EITHER IAM or RBAC allows it, and
// mast reaches the cluster through the GKE MCP server as the KSA's WIF
// principal — so the project-level role setup-wif.sh binds decides
// whether the bindings above bound anything at all. It is bound by a
// shell script and described in a YAML comment, with nothing but this
// test between them. Change one and the other has to keep up.
func TestWifDefaultRoleIsDocumented(t *testing.T) {
	script := readFile(t, wifScript)
	sa := readFile(t, daemonSAT)

	scope, role := wifDefault(t, script)
	if !strings.Contains(sa, role) {
		t.Errorf("%s binds %s by default (WRITE_SCOPE=%s), which %s does not mention — the template's role list is what an operator reads", wifScript, role, scope, daemonSAT)
	}
	if !strings.Contains(script, "WRITE_SCOPE=namespaced") {
		t.Errorf("%s no longer documents the narrowing that makes the RBAC split load-bearing", wifScript)
	}
	// While the default is a cluster-write role, the template has to say
	// so: an operator who reads only the RBAC would otherwise conclude
	// the daemon cannot change kube-system, and it can.
	if slices.Contains(clusterWriteIAM, role) && !strings.Contains(sa, "WARNING") {
		t.Errorf("%s binds cluster-write %s by default and %s carries no warning about it", wifScript, role, daemonSAT)
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
// path is a grant, so the write-verb and secret checks have to see both.
func bindsDaemon(b doc) bool {
	return bindsDaemonSA(b) || bindsDaemonWifUser(b)
}

// bindsDaemonSA reports whether a binding names the daemon's
// ServiceAccount — the in-cluster path.
func bindsDaemonSA(b doc) bool {
	for _, s := range b.Subjects {
		if s.Kind == "ServiceAccount" && s.Name == daemonSA && s.Namespace == releaseNS {
			return true
		}
	}
	return false
}

// bindsDaemonWifUser reports whether a binding names the daemon's
// Workload Identity Federation principal — the GKE MCP path. The whole
// username is checked, not its shape: the chart substitutes the project
// and the namespace into it, and either one wrong is a subject that
// matches nobody.
func bindsDaemonWifUser(b doc) bool {
	for _, s := range b.Subjects {
		if s.Kind == "User" && s.Name == wifUser {
			return true
		}
	}
	return false
}

// workloadDeclaresAWrite reports whether the deployed bundle classifies
// any tool as mutating — the reason a write grant has to exist at all.
func workloadDeclaresAWrite(t *testing.T) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(workloadFile))
	if err != nil {
		t.Fatalf("read %s: %v", workloadFile, err)
	}
	var w struct {
		ToolCatalog struct {
			Tools []struct {
				Name     string `yaml:"name"`
				Mutating bool   `yaml:"mutating"`
			} `yaml:"tools"`
		} `yaml:"tool_catalog"`
	}
	if err := yaml.Unmarshal(data, &w); err != nil {
		t.Fatalf("parse %s: %v", workloadFile, err)
	}
	for _, tool := range w.ToolCatalog.Tools {
		if tool.Mutating {
			return true
		}
	}
	return false
}
