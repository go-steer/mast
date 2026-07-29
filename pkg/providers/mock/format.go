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
//
// Originally derived from go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818

package mock

import (
	adkmodel "google.golang.org/adk/v2/model"
)

// RecordedTurn is the on-disk shape of a single LLM turn consumed by
// the scripted provider.
//
// One RecordedTurn is written per JSONL line. Request is a snapshot
// taken before the inner LLM may have mutated it (Config.Tools is
// commonly appended to). Responses is the full ordered stream of
// LLMResponse values yielded for that turn — typically zero or more
// Partial: true chunks followed by exactly one TurnComplete: true.
//
// Note that adkmodel.LLMRequest.Tools is tagged json:"-" upstream and
// will silently drop on serialization. That's intentional: the inner
// LLM provides tool declarations on replay; recorded Tools would be
// dead weight.
//
// This wire format is shared with core-agent's pkg/recording, whose
// recording wrapper (recording.NewRecorder) produces transcripts in
// exactly this shape. If/when the recorder ports to mast, this type
// and the JSONL read path should be extracted to a mast pkg/recording
// that both packages consume.
type RecordedTurn struct {
	Request   *adkmodel.LLMRequest    `json:"request"`
	Responses []*adkmodel.LLMResponse `json:"responses"`
}
