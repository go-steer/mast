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

package compose

import "github.com/go-steer/mast/pkg/workload"

// ModelOptions is what a bundle says about how mast constructs its
// models: the settings that have to be chosen when the client is built,
// not per call. The zero value is mast's baseline for a caller with no
// bundle to read.
type ModelOptions struct {
	// BuiltinTools gates the provider's server-side built-in tools.
	// Zero is every one of them off.
	BuiltinTools workload.BuiltinTools
	// PromptCache configures Claude prompt caching. Zero is off.
	PromptCache workload.PromptCache
}

// ModelOptionsFor reads b's model settings. A nil bundle is the
// baseline.
func ModelOptionsFor(b *workload.Bundle) ModelOptions {
	if b == nil {
		return ModelOptions{}
	}
	return ModelOptions{BuiltinTools: b.BuiltinTools, PromptCache: b.PromptCache}
}
