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

package mcp

import (
	"encoding/json"
	"fmt"
	"os"
)

// CatalogFileName is the well-known name of the MCP server catalog that
// sits alongside a workload (in a directory bundle) or at the config
// root (in `.agents/` mode). Workload bundles reference servers defined
// here by name via tool_catalog.mcp[].server; this file holds the actual
// transport + auth definitions.
//
// The catalog is a privilege-bearing control-plane file: a stdio server
// entry names a local command mast will execute, so editing it grants
// code execution. It is trusted operator configuration, on par with
// config.json (see pkg/permissions/controlplane.go).
const CatalogFileName = "mcp.json"

// CatalogVersion is the only mcp.json schema version this build accepts.
const CatalogVersion = 1

// Transport kinds a catalog entry may declare.
const (
	// TransportHTTP speaks MCP over a streamable HTTP endpoint (url).
	TransportHTTP = "http"
	// TransportStdio launches a local process and speaks MCP over its
	// stdin/stdout (command + args + env).
	TransportStdio = "stdio"
)

// Environment inheritance modes for a stdio server (ServerConfig.EnvMode).
const (
	// EnvModeInherit (the default, also the zero value "") gives the child
	// the daemon's full environment with the configured Env layered on top.
	EnvModeInherit = "inherit"
	// EnvModeClean starts the child from an empty environment: it sees only
	// the daemon variables named in EnvPassthrough plus the configured Env.
	// Nothing else from the daemon environment leaks into the child.
	EnvModeClean = "clean"
)

// Catalog is a parsed mcp.json: a versioned map of server name to
// definition.
type Catalog struct {
	Version int                     `json:"version"`
	Servers map[string]ServerConfig `json:"servers"`

	// CommandAllowlist, when non-empty, bounds which executables the stdio
	// servers in this catalog may launch: every stdio server's resolved
	// command (after ${VAR} expansion) must appear in this list or the
	// catalog fails validation. Allowlist entries are themselves expanded
	// before comparison, so both sides are matched post-expansion. Empty
	// (the default) imposes no restriction. Defense-in-depth for
	// deployments that want to cap the set of local processes mast can
	// spawn even when the catalog file itself is trusted.
	CommandAllowlist []string `json:"command_allowlist,omitempty"`
}

// ServerConfig defines a single MCP server. The fields that apply depend
// on Transport: HTTP reads URL + Auth; stdio reads Command + Args + Env.
type ServerConfig struct {
	// Transport is the wire mechanism: TransportHTTP or TransportStdio.
	Transport string `json:"transport"`

	// URL is the streamable-HTTP endpoint (TransportHTTP only).
	URL string `json:"url,omitempty"`
	// Auth describes how to authenticate to an HTTP server. nil means
	// the endpoint needs no credentials.
	Auth *AuthConfig `json:"auth,omitempty"`

	// Command is the executable to launch (TransportStdio only). May be a
	// bare name resolved on PATH or an absolute path. Expanded like Args.
	Command string `json:"command,omitempty"`
	// Args are the command's arguments (TransportStdio only). ${VAR}
	// references are expanded against the daemon environment via
	// os.ExpandEnv, so bare $name expands too and there is no escape for a
	// literal $ (see ServerConfig.ResolvedCommand).
	Args []string `json:"args,omitempty"`
	// Env sets environment variables for the child process (TransportStdio
	// only). Values are expanded the same way as Args; a value needing a
	// literal $ should come from the inherited daemon environment instead.
	// How Env combines with the daemon environment depends on EnvMode.
	Env map[string]string `json:"env,omitempty"`

	// EnvMode controls how the child's environment is built (TransportStdio
	// only): "" or "inherit" (default) gives the child the daemon's full
	// environment with Env layered on top; "clean" starts the child from an
	// empty environment so it sees only EnvPassthrough plus Env. Use "clean"
	// to keep unrelated daemon secrets out of a local MCP server — but then
	// list the vars it genuinely needs (e.g. PATH, HOME) in EnvPassthrough.
	EnvMode string `json:"env_mode,omitempty"`
	// EnvPassthrough lists daemon environment variable names to copy into
	// the child verbatim when EnvMode is "clean" (TransportStdio only). It
	// is rejected under inherit mode — where the child already inherits
	// everything, a passthrough list would give a false sense of scoping.
	EnvPassthrough []string `json:"env_passthrough,omitempty"`

	// NoDigest opts this server out of the MCP digest wrap, so its tool
	// responses reach the model exactly as the server sent them (both
	// transports). The daemon-wide switch is --mcp-digest; this is the
	// per-server escape hatch for a server whose responses are small
	// enough that digesting is pure overhead, or whose exact bytes are
	// what the specialist is there to read.
	//
	// It is not validated against anything: opting out of a size
	// optimization cannot be a misconfiguration, and a catalog written
	// for a build with the wrap disabled must still load.
	NoDigest bool `json:"no_digest,omitempty"`
}

// AuthConfig selects an authentication method for an HTTP MCP server.
// Exactly one method should be set; only Google OAuth is wired today.
type AuthConfig struct {
	// GoogleOAuth authenticates via Application Default Credentials.
	GoogleOAuth *GoogleOAuthConfig `json:"google_oauth,omitempty"`
}

// GoogleOAuthConfig configures ADC-based bearer auth for an HTTP server.
type GoogleOAuthConfig struct {
	// Scopes are the OAuth 2.0 scopes to request. Empty defaults to
	// DefaultGKEScope.
	Scopes []string `json:"scopes,omitempty"`
}

// LoadCatalog reads and validates an mcp.json file at path. A missing
// file is an error — callers that may not have a catalog should check
// before calling.
func LoadCatalog(path string) (Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, fmt.Errorf("mcp: read catalog %s: %w", path, err)
	}
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return Catalog{}, fmt.Errorf("mcp: parse catalog %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Catalog{}, fmt.Errorf("mcp: invalid catalog %s: %w", path, err)
	}
	return c, nil
}

// Validate checks the version and every server definition. It reports the
// first problem it finds.
func (c Catalog) Validate() error {
	if c.Version != CatalogVersion {
		return fmt.Errorf("unsupported version %d (want %d)", c.Version, CatalogVersion)
	}
	allow := commandAllowSet(c.CommandAllowlist)
	for name, cfg := range c.Servers {
		if name == "" {
			return fmt.Errorf("server name must not be empty")
		}
		if err := cfg.validate(); err != nil {
			return fmt.Errorf("server %q: %w", name, err)
		}
		if cfg.Transport == TransportStdio && allow != nil {
			if cmd, _ := cfg.ResolvedCommand(); !allow[cmd] {
				return fmt.Errorf("server %q: command %q is not in the catalog command_allowlist", name, cmd)
			}
		}
	}
	return nil
}

// commandAllowSet builds the resolved allowlist lookup, or nil when the
// list is empty (meaning no restriction). Entries are expanded so both
// sides of the membership check are compared post-${VAR}-expansion.
func commandAllowSet(list []string) map[string]bool {
	if len(list) == 0 {
		return nil
	}
	set := make(map[string]bool, len(list))
	for _, c := range list {
		set[os.ExpandEnv(c)] = true
	}
	return set
}

// validate enforces the per-transport required fields.
func (cfg ServerConfig) validate() error {
	switch cfg.Transport {
	case TransportHTTP:
		if cfg.URL == "" {
			return fmt.Errorf("http transport requires a url")
		}
	case TransportStdio:
		if cfg.Command == "" {
			return fmt.Errorf("stdio transport requires a command")
		}
		if err := cfg.validateEnv(); err != nil {
			return err
		}
	case "":
		return fmt.Errorf("missing transport (want %q or %q)", TransportHTTP, TransportStdio)
	default:
		return fmt.Errorf("unknown transport %q (want %q or %q)", cfg.Transport, TransportHTTP, TransportStdio)
	}
	return nil
}

// validateEnv checks the stdio environment-scoping fields. An
// EnvPassthrough list is only meaningful under clean mode; allowing it
// under inherit mode would let an operator believe they had scoped the
// child's environment when the child still inherits everything, so it is
// rejected as a misconfiguration.
func (cfg ServerConfig) validateEnv() error {
	switch cfg.EnvMode {
	case "", EnvModeInherit:
		if len(cfg.EnvPassthrough) > 0 {
			return fmt.Errorf("env_passthrough requires env_mode %q (under %q the child already inherits the daemon environment)", EnvModeClean, EnvModeInherit)
		}
	case EnvModeClean:
		// Any (or no) passthrough is valid; empty means the child sees
		// only the configured env.
	default:
		return fmt.Errorf("unknown env_mode %q (want %q or %q)", cfg.EnvMode, EnvModeInherit, EnvModeClean)
	}
	return nil
}
