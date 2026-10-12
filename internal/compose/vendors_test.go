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

import (
	"testing"

	"github.com/go-steer/mast/internal/taskclass"
)

// The Anthropic small default moved here from internal/providers/anthropic
// with the adapter's move to core-models; the tier table must still agree.
func TestAnthropicSmallDefaultMatchesTheTierTable(t *testing.T) {
	for _, p := range []string{ProviderAnthropic, ProviderAnthropicVertex} {
		if got := taskclass.ModelForTier(p, "small"); got != DefaultAnthropicSmallModel {
			t.Errorf("ModelForTier(%s, small) = %q, want %q", p, got, DefaultAnthropicSmallModel)
		}
	}
}
