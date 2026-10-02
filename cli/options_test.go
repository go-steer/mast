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

package cli

import (
	"testing"

	"google.golang.org/adk/v2/model"

	mastagent "github.com/go-steer/mast/internal/agent"
	internalcli "github.com/go-steer/mast/internal/cli"
)

func apply(opts ...Option) internalcli.Options {
	var ext internalcli.Options
	for _, o := range opts {
		if o.apply != nil {
			o.apply(&ext)
		}
	}
	return ext
}

// WithModels documents first-answer-wins across repeated options, which
// is what lets a main.go layer a specific resolver over a general one.
func TestWithModelsAsksInTheOrderGiven(t *testing.T) {
	first := mastagent.NewEchoModel("first")
	second := mastagent.NewEchoModel("second")
	ext := apply(
		WithModels(func(name string) (model.LLM, bool) { return first, name == "shared" }),
		WithModels(func(name string) (model.LLM, bool) {
			if name == "shared" || name == "only-second" {
				return second, true
			}
			return nil, false
		}),
	)
	if m, ok := ext.Models("shared"); !ok || m != first {
		t.Errorf("a name both answer went to %v, want the first resolver's model", m)
	}
	if m, ok := ext.Models("only-second"); !ok || m != second {
		t.Errorf("a name only the second answers went to %v, want the second's model", m)
	}
	if _, ok := ext.Models("nobody"); ok {
		t.Error("a name no resolver answers was answered")
	}
}

func TestTheZeroOptionAndNilArgumentsAddNothing(t *testing.T) {
	ext := apply(Option{}, WithModels(nil), WithToolset(nil))
	if ext.Models != nil || len(ext.Toolsets) != 0 {
		t.Fatalf("empty options added something: %+v", ext)
	}
}

func TestWithToolsIsOneNamedToolset(t *testing.T) {
	ext := apply(WithTools("tickets"))
	if len(ext.Toolsets) != 1 || ext.Toolsets[0].Name() != "tickets" {
		t.Fatalf("WithTools gave %v, want one toolset named tickets", ext.Toolsets)
	}
}
