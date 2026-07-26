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

package federation

import (
	"fmt"
	"net/url"
	"strings"
)

// Reference is a parsed remote-agent reference per the
// docs/federation-design.md grammar. v0.1 accepts both spellings the
// design corpus uses for skill selection:
//
//	<scheme>://<name>/<skill>     (docs/a2a-design.md)
//	<scheme>://<name>?skill=<s>   (docs/federation-design.md)
//
// Supplying both is an error rather than a silent precedence rule.
// Parsing is standard-URI (docs/federation-design.md open question 1's
// bias: "use standard URI parsing; scheme becomes the adapter
// selector"), which means the name occupies the URI host position and
// is therefore case-insensitive per RFC 3986 — Parse normalizes it to
// lowercase, and agent config names must be lowercase to match (the
// pkg/a2a config loader enforces this). ParseReference applies the
// lowercase normalization itself — net/url preserves host case on
// parse, but two references differing only in name case MUST resolve
// identically, so normalization happens here, once.
type Reference struct {
	// Scheme selects the adapter ("a2a", and in later versions "mast",
	// "http", "grpc"). Always lowercase.
	Scheme string

	// Name is the configured remote-agent name (URI host position).
	// Always lowercase.
	Name string

	// Skill is the optional skill selector. Empty means "the agent's
	// default / sole skill".
	Skill string

	// Raw is the reference string as given.
	Raw string
}

// String returns the canonical `<scheme>://<name>[/<skill>]` form.
func (r Reference) String() string {
	if r.Skill == "" {
		return r.Scheme + "://" + r.Name
	}
	return r.Scheme + "://" + r.Name + "/" + r.Skill
}

// ParseReference parses a remote-agent reference. All parse failures
// wrap ErrInvalidReference.
func ParseReference(raw string) (Reference, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Reference{}, fmt.Errorf("%w: %q: %v", ErrInvalidReference, raw, err)
	}
	if u.Scheme == "" {
		return Reference{}, fmt.Errorf("%w: %q: missing scheme (want <scheme>://<name>[/<skill>])", ErrInvalidReference, raw)
	}
	if u.Host == "" {
		return Reference{}, fmt.Errorf("%w: %q: missing agent name (want <scheme>://<name>[/<skill>])", ErrInvalidReference, raw)
	}
	if u.User != nil || u.Port() != "" {
		// Role/port selectors (mast://worker@fleet, host:port endpoints)
		// are v0.2+ adapter territory; reject rather than drop silently.
		return Reference{}, fmt.Errorf("%w: %q: userinfo/port selectors are not supported in v0.1", ErrInvalidReference, raw)
	}

	pathSkill := strings.Trim(u.Path, "/")
	if strings.Contains(pathSkill, "/") {
		return Reference{}, fmt.Errorf("%w: %q: skill must be a single path segment", ErrInvalidReference, raw)
	}
	querySkill := u.Query().Get("skill")
	if pathSkill != "" && querySkill != "" {
		return Reference{}, fmt.Errorf("%w: %q: skill given both as path (%q) and query (%q); use one", ErrInvalidReference, raw, pathSkill, querySkill)
	}
	skill := pathSkill
	if skill == "" {
		skill = querySkill
	}

	return Reference{
		Scheme: strings.ToLower(u.Scheme),
		Name:   strings.ToLower(u.Hostname()),
		Skill:  skill,
		Raw:    raw,
	}, nil
}
