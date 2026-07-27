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

package permissions

// Settings is the package-local configuration surface for FromConfig.
//
// Port note: in the parent project FromConfig took the loader's
// *config.Config, whose Permissions / PathScope blocks carried these
// fields. mast's pkg/config is the .agents workload/specialist loader
// and has no permissions block yet — wiring the gate into the runtime
// (and therefore into config loading) is its own future workstream
// (see the package doc in denylist.go). Until then, Settings mirrors
// the parent's PermissionsConfig + PathScopeConfig shapes verbatim,
// including JSON tags, so a future .agents/config.json permissions
// block can unmarshal straight into it.
type Settings struct {
	Permissions PermissionsSettings `json:"permissions,omitempty"`
	PathScope   PathScopeSettings   `json:"path_scope,omitempty"`
}

// PermissionsSettings mirrors the parent project's
// config.PermissionsConfig.
type PermissionsSettings struct {
	Mode  string   `json:"mode,omitempty"`  // "ask" | "allow" | "yolo" | "plan" | "acceptEdits"
	Allow []string `json:"allow,omitempty"` // pattern allowlist
	Deny  []string `json:"deny,omitempty"`  // pattern denylist

	// UseBuiltinAllow toggles the built-in conservative read-only
	// allowlist bundle. Defaults to true when nil (the pointer
	// carries an explicit "off" signal vs "unset"). false drops the
	// entire built-in bundle including any opt-ins in
	// BuiltinAllowExtras. See builtin_allow.go for the bundle catalog.
	UseBuiltinAllow *bool `json:"use_builtin_allow,omitempty"`

	// BuiltinAllowExtras names additional built-in bundles to merge
	// on top of read_only when UseBuiltinAllow is on. Unknown names
	// fail at construction time rather than silently dropping
	// permissions. Known bundles: see KnownBundles().
	BuiltinAllowExtras []string `json:"builtin_allow_extras,omitempty"`

	// RequirePlanArtifact enables the plan-first gating pre-check:
	// mutating tool calls are denied until the model calls the
	// record_plan tool. Composes with every Mode — even ModeYolo
	// denies before a plan is recorded.
	RequirePlanArtifact bool `json:"require_plan_artifact,omitempty"`
}

// PathScopeSettings mirrors the parent project's config.PathScopeConfig:
// extra paths that file tools may read/write beyond the project root.
type PathScopeSettings struct {
	Allow      []string              `json:"allow,omitempty"`
	AllowPaths []PathScopeAllowEntry `json:"allow_paths,omitempty"`
}

// PathScopeAllowEntry is one typed allow-list entry. Access is one
// of "r" / "w" / "rw" (long forms "read" / "write" / "readwrite"
// also accepted); empty Access fails validation rather than
// silently broadening to rw. Path uses the same matching rules as
// Allow: exact path, "/.../" subtree, or filepath.Match glob.
type PathScopeAllowEntry struct {
	Path   string `json:"path"`
	Access string `json:"access"`
}

// DefaultSettings returns the conservative baseline: ask mode, no
// user-supplied allow/deny entries, built-in read-only bundle on
// (via UseBuiltinAllow's nil-means-true default).
func DefaultSettings() *Settings {
	return &Settings{
		Permissions: PermissionsSettings{Mode: string(ModeAsk)},
	}
}
