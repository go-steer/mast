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

package usage_test

import (
	"encoding/json"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/providers/usage"
)

// The type has to satisfy the contract the meter reads, and the meter
// must not have to know this package exists to do it.
var _ budget.Detailer = (*usage.Detail)(nil)

func TestAttachPutsTheSidecarWhereTheMeterLooks(t *testing.T) {
	resp := &adkmodel.LLMResponse{}
	usage.Attach(resp, &usage.Detail{CacheWriteTokens: usage.Int64(7)})

	// The meter's own read path: the value under the key implements the
	// interface. A sidecar the meter cannot type-assert is not attached.
	d, ok := resp.CustomMetadata[budget.DetailKey].(budget.Detailer)
	if !ok {
		t.Fatalf("CustomMetadata[%q] = %T, want a budget.Detailer", budget.DetailKey, resp.CustomMetadata[budget.DetailKey])
	}
	b := d.UsageBuckets()
	if b.CacheWriteTokens == nil || *b.CacheWriteTokens != 7 {
		t.Errorf("CacheWriteTokens = %v, want 7", b.CacheWriteTokens)
	}
	if b.CacheReadTokens != nil {
		t.Errorf("CacheReadTokens = %v, want nil — nothing was said about reads", *b.CacheReadTokens)
	}
}

// An adapter that already put something in the map keeps it. The map is
// ADK's and shared.
func TestAttachLeavesOtherKeysAlone(t *testing.T) {
	resp := &adkmodel.LLMResponse{CustomMetadata: map[string]any{"source": "operator"}}
	usage.Attach(resp, &usage.Detail{})
	if resp.CustomMetadata["source"] != "operator" {
		t.Errorf("Attach clobbered an unrelated key: %v", resp.CustomMetadata)
	}
}

// Nothing to say attaches nothing, rather than an empty record that
// reads as "asked and answered".
func TestAttachOfNilIsANoOp(t *testing.T) {
	resp := &adkmodel.LLMResponse{}
	usage.Attach(resp, nil)
	if resp.CustomMetadata != nil {
		t.Errorf("CustomMetadata = %v, want nil", resp.CustomMetadata)
	}
	usage.Attach(nil, &usage.Detail{}) // must not panic
}

// The R3 property, and the reason every count is a pointer: a provider
// that reported zero and a provider that reported nothing are two
// different claims, and they must not serialize the same way either.
func TestZeroAndUnreportedAreDistinguishable(t *testing.T) {
	reportedZero := &usage.Detail{CacheWriteTokens: usage.Int64(0)}
	unreported := &usage.Detail{}

	if b := reportedZero.UsageBuckets(); b.CacheWriteTokens == nil {
		t.Fatal("a reported zero came back as nil; the distinction is gone in the buckets")
	}
	if b := unreported.UsageBuckets(); b.CacheWriteTokens != nil {
		t.Fatal("an unreported bucket came back non-nil")
	}

	zeroJSON, err := json.Marshal(reportedZero)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	silentJSON, err := json.Marshal(unreported)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// omitempty on a *int64 drops nil and keeps a pointer to zero, which
	// is the behaviour the persisted event depends on — an operator
	// reading the eventlog can tell the two apart.
	if !strings.Contains(string(zeroJSON), `"cache_write_tokens":0`) {
		t.Errorf("reported zero serialized as %s, want the field present", zeroJSON)
	}
	if strings.Contains(string(silentJSON), "cache_write_tokens") {
		t.Errorf("unreported bucket serialized as %s, want the field absent", silentJSON)
	}
}

// A nil *Detail answers "nothing said" rather than panicking, so an
// adapter can hand one over without branching.
func TestNilDetailSaysNothing(t *testing.T) {
	var d *usage.Detail
	if got := d.UsageBuckets(); got != (budget.Buckets{}) {
		t.Errorf("(*Detail)(nil).UsageBuckets() = %+v, want the zero Buckets", got)
	}
}

func TestFromEvent(t *testing.T) {
	want := &usage.Detail{ServedModel: "claude-sonnet-5"}
	resp := &adkmodel.LLMResponse{}
	usage.Attach(resp, want)
	ev := &session.Event{LLMResponse: *resp}

	if got := usage.FromEvent(ev); got != want {
		t.Errorf("FromEvent = %v, want the attached detail", got)
	}
	if got := usage.FromEvent(&session.Event{}); got != nil {
		t.Errorf("FromEvent(no sidecar) = %v, want nil", got)
	}
	if got := usage.FromEvent(nil); got != nil {
		t.Errorf("FromEvent(nil) = %v, want nil", got)
	}
}
