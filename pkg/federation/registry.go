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
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry maps reference schemes to Adapters and dispatches
// invocations. Safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry returns a Registry with the given adapters registered.
// It panics on the same conditions Register rejects — construction
// with a duplicate or invalid scheme is a programming error, not a
// runtime condition.
func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{adapters: map[string]Adapter{}}
	for _, a := range adapters {
		if err := r.Register(a); err != nil {
			panic(err)
		}
	}
	return r
}

// Register adds an adapter, keyed by its Scheme. Duplicate or empty
// schemes are rejected.
func (r *Registry) Register(a Adapter) error {
	scheme := strings.ToLower(a.Scheme())
	if scheme == "" {
		return fmt.Errorf("federation: adapter %T declares an empty scheme", a)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.adapters[scheme]; dup {
		return fmt.Errorf("federation: adapter scheme %q registered twice", scheme)
	}
	r.adapters[scheme] = a
	return nil
}

// Adapter returns the adapter registered for scheme, or an error
// wrapping ErrUnknownScheme.
func (r *Registry) Adapter(scheme string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[strings.ToLower(scheme)]
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)", ErrUnknownScheme, scheme, r.schemesLocked())
	}
	return a, nil
}

// Invoke parses raw, resolves the adapter by scheme, and dispatches.
// Same error contract as Adapter.Invoke: dispatch-time failures here,
// execution failures from Handle.Wait.
func (r *Registry) Invoke(ctx context.Context, raw string, inputs map[string]any, opts InvokeOptions) (Handle, error) {
	ref, err := ParseReference(raw)
	if err != nil {
		return nil, err
	}
	a, err := r.Adapter(ref.Scheme)
	if err != nil {
		return nil, err
	}
	return a.Invoke(ctx, ref, inputs, opts)
}

// schemesLocked renders the registered scheme set for error messages.
// Caller holds at least the read lock.
func (r *Registry) schemesLocked() string {
	if len(r.adapters) == 0 {
		return "none"
	}
	schemes := make([]string, 0, len(r.adapters))
	for s := range r.adapters {
		schemes = append(schemes, s)
	}
	sort.Strings(schemes)
	return strings.Join(schemes, ", ")
}
