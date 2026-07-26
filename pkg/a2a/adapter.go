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
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-steer/mast/pkg/federation"
)

// Scheme is the federation reference scheme this adapter serves:
// a2a://<name>/<skill> (docs/federation-design.md reference grammar).
const Scheme = "a2a"

// Adapter is the A2A protocol adapter for the federation registry —
// the v0.1 slice of docs/federation-design.md's "A2A adapter": static
// references only; registry-discovered references (a2a://<registry>/
// <agent-id>) are v0.2.
type Adapter struct {
	clients map[string]*Client
}

var _ federation.Adapter = (*Adapter)(nil)

// NewAdapter builds the adapter from static agent configs (normally
// pkg/config's .agents/a2a/*.yaml scan). opts apply to every client.
func NewAdapter(cfgs []AgentConfig, opts ...ClientOption) (*Adapter, error) {
	a := &Adapter{clients: make(map[string]*Client, len(cfgs))}
	for _, cfg := range cfgs {
		if prev, dup := a.clients[cfg.Name]; dup {
			return nil, fmt.Errorf("a2a: agent name collision: %q defined by both %s and %s", cfg.Name, prev.cfg.Filename, cfg.Filename)
		}
		c, err := NewClient(cfg, opts...)
		if err != nil {
			return nil, err
		}
		a.clients[cfg.Name] = c
	}
	return a, nil
}

// Scheme implements federation.Adapter.
func (a *Adapter) Scheme() string { return Scheme }

// Invoke implements federation.Adapter. Per the frozen contract it
// returns an error only for dispatch-time failures (unknown agent);
// the synchronous remote call runs inside Invoke — v0.1 blocks to a
// bounded timeout — and its outcome, success or failure, surfaces
// from the returned Handle's Wait.
func (a *Adapter) Invoke(ctx context.Context, ref federation.Reference, inputs map[string]any, opts federation.InvokeOptions) (federation.Handle, error) {
	c, ok := a.clients[ref.Name]
	if !ok {
		return nil, fmt.Errorf("%w: %q (configured a2a agents: %s)", federation.ErrUnknownAgent, ref.Name, a.names())
	}
	res, err := c.Send(ctx, ref.Skill, inputs, opts.Timeout)
	return federation.NewResolvedHandle(res, err), nil
}

func (a *Adapter) names() string {
	if len(a.clients) == 0 {
		return "none"
	}
	names := make([]string, 0, len(a.clients))
	for n := range a.clients {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
