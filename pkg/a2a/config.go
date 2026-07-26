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

package a2a

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultTimeout bounds a Send when neither the per-agent config nor
// the caller supplies one. v0.1 calls block to a bounded timeout —
// long-running remote tasks need programmatic pause, which is v0.2 per
// docs/durable-execution-design.md phasing.
const DefaultTimeout = 120 * time.Second

// AuthTypeBearer is the only auth type the v0.1 client supports.
// google-iam (docs/a2a-design.md static-registration example) joins in
// v0.2 alongside the pluggable token-resolver surface.
const AuthTypeBearer = "bearer"

// AuthConfig is the static-registration auth block. Tokens are
// env-var references, never file-embedded (docs/a2a-design.md).
type AuthConfig struct {
	// Type is the auth mechanism; v0.1: "bearer" only.
	Type string `yaml:"type"`

	// TokenEnv names the environment variable holding the bearer
	// token. Resolved at request time, not load time, so rotation
	// does not require a config reload.
	TokenEnv string `yaml:"token_env"`
}

// AgentConfig is one static A2A agent registration from
// .agents/a2a/<name>.yaml (docs/a2a-design.md, "Static registration").
type AgentConfig struct {
	// Name is the reference name: a2a://<name>/<skill>. Must be
	// lowercase because the name travels in the URI host position of a
	// federation reference, where RFC 3986 case-insensitivity means
	// parsers normalize to lowercase (see federation.ParseReference).
	Name string `yaml:"name"`

	// AgentCardURL locates the agent card. A base URL (empty or "/"
	// path) gets WellKnownCardPath appended. The JSON-RPC endpoint is
	// then resolved from the card.
	AgentCardURL string `yaml:"agent_card_url,omitempty"`

	// Endpoint is the JSON-RPC endpoint, bypassing card discovery.
	// When both Endpoint and AgentCardURL are set, Endpoint wins for
	// transport and the card is still fetched for skill validation.
	Endpoint string `yaml:"endpoint,omitempty"`

	// Skills is the subset of the agent's skills mast may invoke;
	// empty = all (docs/a2a-design.md).
	Skills []string `yaml:"skills,omitempty"`

	// Auth is optional; absent means unauthenticated calls.
	Auth *AuthConfig `yaml:"auth,omitempty"`

	// TimeoutSeconds bounds each invocation; 0 = DefaultTimeout.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`

	// Filename records provenance for error messages.
	Filename string `yaml:"-"`
}

// Timeout returns the effective invocation bound.
func (c AgentConfig) Timeout() time.Duration {
	if c.TimeoutSeconds > 0 {
		return time.Duration(c.TimeoutSeconds) * time.Second
	}
	return DefaultTimeout
}

// nameRE enforces lowercase reference-safe names (see AgentConfig.Name).
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// Validate checks an AgentConfig for load-time errors. All errors are
// fatal per the config-layout v0.1 rule (fail fast on invalid config).
func (c AgentConfig) Validate() error {
	where := c.Filename
	if where == "" {
		where = "(programmatic)"
	}
	if c.Name == "" {
		return fmt.Errorf("a2a: %s: missing required field name", where)
	}
	if !nameRE.MatchString(c.Name) {
		return fmt.Errorf("a2a: %s: name %q must be lowercase alphanumeric with interior [._-] (it is matched against the case-insensitive host of an a2a:// reference)", where, c.Name)
	}
	if c.AgentCardURL == "" && c.Endpoint == "" {
		return fmt.Errorf("a2a: %s (%s): one of agent_card_url or endpoint is required", where, c.Name)
	}
	if c.Auth != nil {
		if c.Auth.Type != AuthTypeBearer {
			return fmt.Errorf("a2a: %s (%s): auth.type %q is not supported in v0.1 (only %q; google-iam is v0.2)", where, c.Name, c.Auth.Type, AuthTypeBearer)
		}
		if c.Auth.TokenEnv == "" {
			return fmt.Errorf("a2a: %s (%s): auth.token_env is required for bearer auth (tokens are env-var references, never file-embedded)", where, c.Name)
		}
	}
	if c.TimeoutSeconds < 0 {
		return fmt.Errorf("a2a: %s (%s): timeout_seconds must be >= 0", where, c.Name)
	}
	return nil
}

// LoadDir loads every *.yaml / *.yml in dir (flat, non-recursive, per
// the pkg/config scan rules). A missing dir yields zero entries. Any
// invalid file is a fatal load error. Name-collision checking across
// files is the caller's job (pkg/config does it alongside its other
// same-directory collision checks).
func LoadDir(dir string) ([]AgentConfig, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("a2a: read dir %q: %w", dir, err)
	}
	var out []AgentConfig
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		cfg, err := loadFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	return out, nil
}

func loadFile(path string) (AgentConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return AgentConfig{}, fmt.Errorf("a2a: read %q: %w", path, err)
	}
	var cfg AgentConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // reject typo'd keys at load time
	if err := dec.Decode(&cfg); err != nil {
		return AgentConfig{}, fmt.Errorf("a2a: parse %q: %w", path, err)
	}
	cfg.Filename = path
	if err := cfg.Validate(); err != nil {
		return AgentConfig{}, err
	}
	return cfg, nil
}
