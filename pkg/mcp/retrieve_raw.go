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

// Derived from go-steer/core-agent's pkg/tools/retrieve.go, ported to
// ADK v2 and moved next to the wrap that fills the store (#221).

package mcp

import (
	"errors"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/mast/pkg/digest"
)

// RetrieveRawToolName is the model-facing name of the escape hatch.
const RetrieveRawToolName = "retrieve_raw"

// retrieveRawDescription is the prose the model reads when it decides
// whether to call this tool, and most of it is spent talking it out of
// doing so.
//
// That is not padding. Upstream measured the failure on a live demo
// (2026-07-17): Flash called retrieve_raw to "double-check" a digest,
// re-inflated ~28k tokens, and ran the same triage roughly six times
// more expensive than it had the day before. A wrap that saves context
// and a tool that hands it straight back cancel out unless the tool's
// description does the work. Softening any of these clauses brings the
// cost spike back; pkg/mcp's tests pin them.
const retrieveRawDescription = "Fetch the raw, un-digested payload for a prior tool call whose response arrived digested. The call_id is what the digest wrapper stamped onto the compressed response you saw. Treat the digest as authoritative by default — DO NOT call retrieve_raw to spot-check, cross-verify, or 'see what was truncated' when the digest already answers your question. Every call re-inflates the full payload back into your context, undoing the wrap's savings (which is the point: a call that would have burned 12k tokens uncached still burns those 12k when you retrieve_raw the same content). Only call when the digest itself signals a load-bearing field was dropped (the digest_meta will include a truncated-field marker) AND you need that specific truncated content to proceed. When the digest is ambiguous but the raw isn't obviously needed, prefer a narrower follow-up call to the underlying tool over re-inflating the whole payload. Returns the raw text and its byte size."

type retrieveRawArgs struct {
	CallID string `json:"call_id" jsonschema:"the call_id stamped onto the digest by the digest wrapper — surfaced as call_id in the digested tool response"`
}

// retrieveRawResult is what the model gets back. Bytes is the size of
// the raw payload, which the model needs to decide whether to slice
// before handing any of it to the next step.
type retrieveRawResult struct {
	Raw   string `json:"raw"`
	Bytes int    `json:"bytes"`
}

// NewRetrieveRawTool exposes digest.Store.Get to the model as a tool.
//
// Digesting is only safe when the model can get the original back, so
// this tool is the other half of WithDigest rather than an optional
// extra: the wiring site registers a store and this tool together, or
// neither. A nil store is a construction error for the same reason —
// a registered tool that refuses every call teaches the model nothing
// except that the escape hatch does not work.
//
// The handler never returns a Go error. An unknown call_id or a broken
// store comes back as a normal tool response whose `raw` carries an
// "(error: ...)" prefix, because the model has to stay in the loop to
// recover: aborting the turn over a failed spot-check is a worse
// outcome than telling it the spot-check is unavailable.
func NewRetrieveRawTool(store digest.Store) (tool.Tool, error) {
	if store == nil {
		return nil, fmt.Errorf("mcp: NewRetrieveRawTool: a digest store is required")
	}
	return functiontool.New(
		functiontool.Config{Name: RetrieveRawToolName, Description: retrieveRawDescription},
		retrieveRawFunc(store),
	)
}

// retrieveRawFunc is the handler as a plain function so tests can
// exercise it without going through functiontool's reflection layer.
func retrieveRawFunc(store digest.Store) func(adkagent.Context, retrieveRawArgs) (retrieveRawResult, error) {
	return func(ctx adkagent.Context, in retrieveRawArgs) (retrieveRawResult, error) {
		if in.CallID == "" {
			return retrieveRawResult{
				Raw: "(error: retrieve_raw requires a non-empty call_id)",
			}, nil
		}
		raw, err := store.Get(ctx, in.CallID)
		if err != nil {
			// "I do not have that" and "I am broken" call for
			// different next moves from the model — try another id
			// versus stop trying — so they are different messages.
			if errors.Is(err, digest.ErrNotFound) {
				return retrieveRawResult{
					Raw: fmt.Sprintf("(error: no raw payload stored for call_id %q — the digest may pre-date store wiring, or the id may be typo'd)", in.CallID),
				}, nil
			}
			return retrieveRawResult{
				Raw: fmt.Sprintf("(error: store failed to fetch %q: %v)", in.CallID, err),
			}, nil
		}
		return retrieveRawResult{Raw: string(raw), Bytes: len(raw)}, nil
	}
}
