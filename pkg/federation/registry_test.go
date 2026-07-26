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
	"errors"
	"testing"
)

// fakeAdapter records the last invocation and returns a canned result.
type fakeAdapter struct {
	scheme  string
	lastRef Reference
	lastIn  map[string]any
	res     *Result
	err     error
}

func (f *fakeAdapter) Scheme() string { return f.scheme }

func (f *fakeAdapter) Invoke(_ context.Context, ref Reference, inputs map[string]any, _ InvokeOptions) (Handle, error) {
	f.lastRef = ref
	f.lastIn = inputs
	return NewResolvedHandle(f.res, f.err), nil
}

func TestRegistryDispatchesByScheme(t *testing.T) {
	fa := &fakeAdapter{scheme: "a2a", res: &Result{State: "completed", Text: "ok"}}
	reg := NewRegistry(fa)

	h, err := reg.Invoke(context.Background(), "a2a://sample/skill", map[string]any{"k": "v"}, InvokeOptions{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res, err := h.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q, want ok", res.Text)
	}
	if fa.lastRef.Name != "sample" || fa.lastRef.Skill != "skill" {
		t.Errorf("adapter saw ref %+v", fa.lastRef)
	}
	if fa.lastIn["k"] != "v" {
		t.Errorf("adapter saw inputs %v", fa.lastIn)
	}
}

func TestRegistryUnknownScheme(t *testing.T) {
	reg := NewRegistry(&fakeAdapter{scheme: "a2a"})
	if _, err := reg.Invoke(context.Background(), "mast://fleet/x", nil, InvokeOptions{}); !errors.Is(err, ErrUnknownScheme) {
		t.Fatalf("err = %v, want ErrUnknownScheme", err)
	}
}

func TestRegistryInvalidReference(t *testing.T) {
	reg := NewRegistry(&fakeAdapter{scheme: "a2a"})
	if _, err := reg.Invoke(context.Background(), "not a reference", nil, InvokeOptions{}); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("err = %v, want ErrInvalidReference", err)
	}
}

func TestRegistryDuplicateSchemeRejected(t *testing.T) {
	reg := NewRegistry(&fakeAdapter{scheme: "a2a"})
	if err := reg.Register(&fakeAdapter{scheme: "A2A"}); err == nil {
		t.Fatal("Register(duplicate scheme) succeeded, want error")
	}
}

func TestResolvedHandleContract(t *testing.T) {
	// The frozen v0.1 Handle contract: Events is non-nil and already
	// closed, Wait is idempotent, Cancel is a no-op.
	h := NewResolvedHandle(&Result{State: "completed"}, nil)
	select {
	case _, open := <-h.Events():
		if open {
			t.Fatal("Events() delivered an event on a resolved handle")
		}
	default:
		t.Fatal("Events() channel is not closed on a resolved handle")
	}
	if err := h.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	for i := 0; i < 2; i++ {
		res, err := h.Wait(context.Background())
		if err != nil || res.State != "completed" {
			t.Fatalf("Wait #%d = (%v, %v)", i+1, res, err)
		}
	}

	herr := NewResolvedHandle(nil, ErrRemoteFailed)
	if _, err := herr.Wait(context.Background()); !errors.Is(err, ErrRemoteFailed) {
		t.Fatalf("error handle Wait = %v, want ErrRemoteFailed", err)
	}
}
