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

// examples/deploy/terraform is a third way to install mast, alongside
// scripts/setup-wif.sh and the chart, and the first two now describe the
// same IAM twice. `terraform test` checks the module against itself —
// that write_scope decides the role set, that one namespace reaches both
// halves — but it cannot see the script, the chart, or a template, so
// everything in this file is a claim that spans two artifacts and can
// therefore only rot in one of them.
//
// Three of these guards also pin a property the HCL tests structurally
// cannot assert at all: an assertion in a .tftest.hcl reads values, and
// "this resource is google_project_iam_member and not
// google_project_iam_binding" is not a value — it is the difference
// between a module that adds a binding to somebody else's project and
// one that takes the project's IAM policy over.
//
// (The package comment is in render_test.go.)

package charts

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	tfRoot    = "../examples/deploy/terraform"
	tfWifMain = tfRoot + "/modules/wif/main.tf"
	tfHelpers = "mast/templates/_helpers.tpl"
)

// authoritativeIAM is the set of resources that replace an IAM policy
// instead of adding to it. google_project_iam_binding owns a role across
// every member in the project; google_project_iam_policy owns the whole
// policy. Either one, in a module an operator drops into a project that
// already runs things, deletes bindings this module never created — and
// it does it on the first apply, silently, because Terraform considers
// that convergence rather than damage.
var authoritativeIAM = []string{
	"google_project_iam_binding",
	"google_project_iam_policy",
	"google_service_account_iam_binding",
	"google_service_account_iam_policy",
	"google_folder_iam_policy",
	"google_organization_iam_policy",
}

// secretCreators write a value into Terraform state. The module names
// the daemon's bearer Secrets and creates neither: a token Terraform
// generates or is handed is stored in the state file in plaintext, and a
// state bucket is usually readable by more people than a Helm release
// is. This is the list of ways that decision gets quietly reversed by
// somebody adding a convenience.
var secretCreators = []string{
	"random_password",
	"random_string",
	"random_id",
	"kubernetes_secret",
	"google_secret_manager_secret_version",
}

// TestTerraformWifIsAdditivePerMember pins the resource choice that the
// module's own tests cannot see.
//
// modules/wif's narrowing — flip write_scope back to "namespaced" and
// the roles/container.admin binding is gone — is what the module exists
// for, and wif.tftest.hcl asserts it by reading the bound role set off
// the resources. But that only proves the set shrank. That the removed
// key is DESTROYED rather than orphaned, and that destroying it removes
// one member's grant rather than every member's, is a property of which
// resource type was used, and no assertion in HCL can read a resource's
// type. So it is read here, out of the source.
func TestTerraformWifIsAdditivePerMember(t *testing.T) {
	tf := readFile(t, tfWifMain)

	if !strings.Contains(tf, `resource "google_project_iam_member" "`) {
		t.Errorf("%s binds no google_project_iam_member — the narrowing in wif.tftest.hcl only removes one principal's grant because the bindings are per-member", tfWifMain)
	}
	for _, path := range terraformFiles(t) {
		body := readFile(t, path)
		for _, res := range authoritativeIAM {
			if strings.Contains(body, `resource "`+res+`" "`) {
				t.Errorf("%s declares %s: it is authoritative, so an apply deletes every binding in the operator's project that this module did not create", path, res)
			}
		}
	}
}

// TestTerraformCreatesNoSecret pins the other decision that is invisible
// to the module's own tests: release.tftest.hcl asserts that
// required_secrets NAMES both bearer Secrets, which stays true whether or
// not something else in the configuration also creates them.
func TestTerraformCreatesNoSecret(t *testing.T) {
	for _, path := range terraformFiles(t) {
		body := readFile(t, path)
		for _, res := range secretCreators {
			if strings.Contains(body, `resource "`+res+`" "`) {
				t.Errorf("%s declares %s: a token Terraform authors is written to the state file in plaintext, which is a wider audience than the Secret it would populate", path, res)
			}
		}
	}
}

// TestTerraformNamesTheChartsServiceAccount couples the hardcoded KSA in
// the module to the chart's.
//
// The daemon's ServiceAccount name is fixed by _helpers.tpl rather than
// prefixed with the release name, precisely so the WIF principal does
// not depend on what somebody typed after `helm install`. That makes it
// safe for the Terraform module to hardcode it too — and makes the two
// copies a pair that has to move together. They are in different
// languages in different directories, so nothing but this notices.
func TestTerraformNamesTheChartsServiceAccount(t *testing.T) {
	chartSA, ok := definedValue(readFile(t, tfHelpers), "mast.daemonSA")
	if !ok {
		t.Fatalf("%s no longer defines mast.daemonSA as a literal — if the name became dynamic, the Terraform module cannot hardcode it", tfHelpers)
	}
	tfSA, ok := quotedAfter(readFile(t, tfWifMain), "ksa_name = ")
	if !ok {
		t.Fatalf("%s no longer sets a literal ksa_name", tfWifMain)
	}
	if chartSA != tfSA {
		t.Errorf("the chart creates ServiceAccount %q and the WIF principal in %s names %q — the principal would be bound for an identity that does not exist", chartSA, tfWifMain, tfSA)
	}
	if chartSA != daemonSA {
		t.Errorf("the chart's daemon ServiceAccount is now %q; these tests and the docs still say %q", chartSA, daemonSA)
	}
}

// TestTerraformAndSetupWifBuildTheSamePrincipal compares the two
// identity strings character for character, with only the four
// substituted values held apart.
//
// A principal that differs from the script's is not a test failure an
// operator would ever see as one: IAM accepts a binding for a principal
// that identifies nobody, so the apply succeeds and every tool call
// comes back Forbidden later, mid-incident, with both artifacts reading
// correctly on their own. That is #290 in a third artifact.
func TestTerraformAndSetupWifBuildTheSamePrincipal(t *testing.T) {
	script, ok := shellAssignment(readFile(t, wifScript), "KSA_PRINCIPAL=")
	if !ok {
		t.Fatalf("%s no longer assigns KSA_PRINCIPAL in a shape this test can read", wifScript)
	}
	tf, ok := quotedAfter(readFile(t, tfWifMain), "principal = ")
	if !ok {
		t.Fatalf("%s no longer sets a literal principal", tfWifMain)
	}
	if got, want := holes.Replace(tf), holes.Replace(script); got != want {
		t.Errorf("the module and %s build different WIF principals, so one of them binds nobody:\n  terraform: %s\n  script:    %s", wifScript, got, want)
	}
}

// holes normalizes the two principal templates down to the shape they
// share: the same five path segments around the same four substituted
// values. Everything else — a missing /locations/global, a pool named
// for the project number instead of the project ID — survives the
// replacement and fails the comparison, which is the point.
var holes = strings.NewReplacer(
	"${PROJECT_NUMBER}", "<number>",
	"${PROJECT_ID}", "<project>",
	"${NAMESPACE}", "<namespace>",
	"${KSA_NAME}", "<ksa>",
	"${local.project_number}", "<number>",
	"${var.project_id}", "<project>",
	"${var.namespace}", "<namespace>",
	"${local.ksa_name}", "<ksa>",
)

// TestTerraformAndSetupWifGrantTheSameRoles holds the module to the
// script for both write scopes.
//
// The two are meant to be interchangeable — the module exists for the
// one thing the script cannot do, not for a different grant — and an
// operator who moves from one to the other gets whatever the second one
// binds. A role the module drops is an agent that stops working; a role
// it adds is a privilege nobody reviewed, in a project where the script
// was what got reviewed.
func TestTerraformAndSetupWifGrantTheSameRoles(t *testing.T) {
	script := readFile(t, wifScript)
	tf := readFile(t, tfWifMain)

	for _, scope := range []string{"namespaced", "cluster-admin"} {
		t.Run(scope, func(t *testing.T) {
			want := scriptProjectRoles(t, script, scope)
			got := terraformProjectRoles(t, tf, scope)
			if !slices.Equal(got, want) {
				t.Errorf("write_scope=%s binds %v and WRITE_SCOPE=%s binds %v", scope, got, scope, want)
			}
		})
	}

	// The fourth binding is not a project role and lives in its own
	// resource, so the loop above cannot see it. Without it the daemon
	// cannot impersonate the node service account and Workload Identity
	// Federation never completes.
	if !strings.Contains(tf, `"roles/iam.serviceAccountUser"`) {
		t.Errorf("%s binds no roles/iam.serviceAccountUser on the node service account, which %s does", tfWifMain, wifScript)
	}
}

// TestTerraformAndSetupWifEnableTheSameAPIs is the same argument one
// layer down: an API the module forgets is an apply that succeeds and a
// daemon that cannot reach Vertex or exchange a token.
func TestTerraformAndSetupWifEnableTheSameAPIs(t *testing.T) {
	want := allQuotedAfter(readFile(t, wifScript), `enable_api "`)
	got := hclList(t, readFile(t, tfWifMain), "apis = var.enable_apis ? toset([")

	slices.Sort(want)
	slices.Sort(got)
	if len(want) == 0 {
		t.Fatalf("%s enables no APIs — this test would pass on anything", wifScript)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the module enables %v and %s enables %v", got, wifScript, want)
	}
}

// scriptProjectRoles is what `bind_project_role` is called with once the
// scope's case arm is resolved.
func scriptProjectRoles(t *testing.T, script, scope string) []string {
	t.Helper()
	container, ok := shellAssignment(script, scope+") CONTAINER_ROLE=")
	if !ok {
		t.Fatalf("%s has no case arm binding a CONTAINER_ROLE for WRITE_SCOPE=%s", wifScript, scope)
	}
	roles := allQuotedAfter(script, `bind_project_role "`)
	if len(roles) == 0 {
		t.Fatalf("%s calls bind_project_role nowhere this test can read", wifScript)
	}
	for i, r := range roles {
		if r == "${CONTAINER_ROLE}" {
			roles[i] = container
		}
	}
	slices.Sort(roles)
	return roles
}

// terraformProjectRoles is the local.project_roles list with the
// write_scope ternary resolved for one scope.
func terraformProjectRoles(t *testing.T, tf, scope string) []string {
	t.Helper()
	m := containerRoleTernary.FindStringSubmatch(tf)
	if m == nil {
		t.Fatalf("%s no longer resolves container_role from write_scope in a shape this test can read", tfWifMain)
	}
	container := m[3]
	if scope == m[1] {
		container = m[2]
	}

	roles := hclList(t, tf, "project_roles = [")
	var resolved int
	for i, r := range roles {
		if r == "local.container_role" {
			roles[i] = container
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("%s's project_roles referenced local.container_role %d times; expected exactly one", tfWifMain, resolved)
	}
	slices.Sort(roles)
	return roles
}

// containerRoleTernary matches
// `container_role = var.write_scope == "X" ? "role-if-X" : "role-otherwise"`.
var containerRoleTernary = regexp.MustCompile(
	`container_role\s*=\s*var\.write_scope\s*==\s*"([^"]+)"\s*\?\s*"([^"]+)"\s*:\s*"([^"]+)"`)

// hclList reads the entries of a bracketed list that starts at prefix,
// returning quoted strings verbatim and bare expressions (e.g.
// local.container_role) as their source text, so a caller can resolve
// them.
func hclList(t *testing.T, body, prefix string) []string {
	t.Helper()
	_, after, ok := strings.Cut(body, prefix)
	if !ok {
		t.Fatalf("no list starting %q in the module source", prefix)
	}
	block, _, ok := strings.Cut(after, "]")
	if !ok {
		t.Fatalf("the list starting %q is never closed", prefix)
	}

	var out []string
	for _, line := range strings.Split(block, "\n") {
		// A trailing `# comment` is documentation, not an entry.
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		entry := strings.TrimSuffix(strings.TrimSpace(line), ",")
		if entry == "" {
			continue
		}
		out = append(out, strings.Trim(entry, `"`))
	}
	return out
}

// definedValue returns the body of a single-line Helm `define`, which is
// how the chart pins a name it refuses to make dynamic.
func definedValue(tpl, name string) (string, bool) {
	_, after, ok := strings.Cut(tpl, `{{- define "`+name+`" -}}`)
	if !ok {
		return "", false
	}
	value, _, ok := strings.Cut(after, "{{-")
	return strings.TrimSpace(value), ok
}

// quotedAfter returns the first double-quoted string following prefix.
func quotedAfter(body, prefix string) (string, bool) {
	_, after, ok := strings.Cut(body, prefix)
	if !ok {
		return "", false
	}
	after = strings.TrimPrefix(after, `"`)
	value, _, ok := strings.Cut(after, `"`)
	return value, ok
}

// allQuotedAfter returns every double-quoted string following each
// occurrence of prefix, in source order.
func allQuotedAfter(body, prefix string) []string {
	var out []string
	rest := body
	for {
		_, after, ok := strings.Cut(rest, prefix)
		if !ok {
			return out
		}
		value, tail, ok := strings.Cut(after, `"`)
		if !ok {
			return out
		}
		out = append(out, value)
		rest = tail
	}
}

// terraformFiles is every .tf file in the module tree. The scans above
// walk it rather than naming files, because the way an authoritative IAM
// resource or a generated password arrives is a new file.
func terraformFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(tfRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".tf" {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", tfRoot, err)
	}
	if len(out) == 0 {
		t.Fatalf("no .tf files under %s — these scans would pass on an empty tree", tfRoot)
	}
	return out
}
