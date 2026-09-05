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

package approval

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	adkagent "google.golang.org/adk/v2/agent"
)

// Prior-state capture, exercised through the real plugin under a real
// runner and a real session store rather than by calling take directly.
//
// The claim being tested is not "capture works" — capture_test.go covers
// that. It is that there is no way for a mutating call to reach
// execution without passing the capture door. A call gets through this
// gate four ways, and each is a separate branch in the plugin:
//
//	apply     — the policy never asks anybody
//	approved  — an operator said yes to the model's arguments
//	edited    — an operator said yes to arguments of their own
//	granted   — an earlier change-set approval speaks for this call
//
// Three call sites cover them, because approved and edited converge
// before the forward call. A fifth path added later without a capture
// door would be a mutating call that changes state mast recorded nothing
// about — silently, and only for that path. That is why these are four
// tests rather than one: a shared helper asserting "some call was
// captured" would pass with three doors and a hole.

// capturingProbe is a CaptureRules wired for a gate probe, with the read
// observable so a test can tell "not captured" from "captured empty".
type capturingProbe struct {
	rules *CaptureRules
	reads []string
	prior map[string]any
	err   error
}

// newGateCaptures declares the standard scenario for every tool: read
// the deployment, keep spec.replicas, and offer the scale call that puts
// it back.
func newGateCaptures(t *testing.T) *capturingProbe {
	t.Helper()
	p := &capturingProbe{prior: priorState()}
	p.rules = &CaptureRules{
		For: func(string) (*Capture, error) { return scaleCapture(), nil },
		Read: func(_ adkagent.Context, name string, args map[string]any) (map[string]any, error) {
			p.reads = append(p.reads, CallKey(name, args))
			if p.err != nil {
				return nil, p.err
			}
			out := map[string]any{}
			for k, v := range p.prior {
				out[k] = v
			}
			return out, nil
		},
		Schema: func(string) (*jsonschema.Schema, error) { return mcpShaped(), nil },
		Now:    fixedClock(t),
	}
	return p
}

// captureRecords reads every prior-state record out of durable session
// state, ordered by the call they belong to so assertions are stable.
func captureRecords(t *testing.T, state map[string]any) []CaptureRecord {
	t.Helper()
	var out []CaptureRecord
	for k, v := range state {
		if !strings.HasPrefix(k, CaptureStateKeyPrefix) {
			continue
		}
		r, err := DecodeCapture(v)
		if err != nil {
			t.Fatalf("state[%q] is not a capture record: %v", k, err)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// wantOneCapture asserts exactly one record and returns it.
func wantOneCapture(t *testing.T, state map[string]any) CaptureRecord {
	t.Helper()
	recs := captureRecords(t, state)
	if len(recs) != 1 {
		t.Fatalf("capture records = %d, want exactly 1: %+v", len(recs), recs)
	}
	return recs[0]
}

// wantRevertsTo asserts the recorded undo restores the prior replica
// count — the one assertion that distinguishes a real revert path from a
// record that would re-apply the change.
func wantRevertsTo(t *testing.T, rec CaptureRecord, replicas float64) {
	t.Helper()
	if rec.Revert == nil {
		t.Fatalf("record for %s carries no revert", rec.Key)
	}
	if got := rec.Revert.Arguments["replicas"]; got != replicas {
		t.Errorf("revert would set replicas to %v, want the prior %v", got, replicas)
	}
}

// --- the four paths -------------------------------------------------

func TestCapture_OnTheApplyPath(t *testing.T) {
	// No operator is asked at all, which makes this the path where a
	// missing capture is least likely to be noticed and most costly: an
	// unattended workload changing things with no record of what they
	// were.
	caps := newGateCaptures(t)
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationApply, captures: caps.rules})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
	if len(caps.reads) != 1 {
		t.Fatalf("capture read ran %d time(s), want 1: %v", len(caps.reads), caps.reads)
	}
	rec := wantOneCapture(t, p.state)
	if got := rec.Arguments["replicas"]; got != float64(10) {
		t.Errorf("record arguments[replicas] = %v, want the model's 10", got)
	}
	wantRevertsTo(t, rec, 3)
}

func TestCapture_OnTheApprovedPath(t *testing.T) {
	caps := newGateCaptures(t)
	p := runGateProbe(t, gateProbeConfig{
		policy:   OnMutationRequireApproval,
		respond:  approve(map[string]any{"verdict": "approve", "approver": "user:sre-oncall"}),
		captures: caps.rules,
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
	rec := wantOneCapture(t, p.state)
	wantRevertsTo(t, rec, 3)

	// The capture is taken when the call is about to run, not when it is
	// parked. An operator can think for an hour; a snapshot from before
	// they started is a record of a world that has since moved.
	if len(caps.reads) != 1 {
		t.Errorf("capture read ran %d time(s), want 1 — once, on the way to execution: %v", len(caps.reads), caps.reads)
	}
}

func TestCapture_OnTheEditedPath_RecordsWhatTheOperatorRan(t *testing.T) {
	// The path most likely to record the wrong thing. A record of what
	// was overwritten has to name the call that overwrote it, and on an
	// edit that is the operator's arguments, not the model's.
	caps := newGateCaptures(t)
	p := runGateProbe(t, gateProbeConfig{
		policy:   OnMutationRequireApproval,
		respond:  approve(editVerdict(map[string]any{"deployment": "api", "replicas": 2})),
		captures: caps.rules,
	})

	if len(p.executions) != 1 || p.executions[0].Replicas != 2 {
		t.Fatalf("executions = %+v, want the operator's 2 replicas", p.executions)
	}
	rec := wantOneCapture(t, p.state)
	if got := rec.Arguments["replicas"]; got != float64(2) {
		t.Errorf("record arguments[replicas] = %v, want the operator's 2 — the record names the model's rejected call", got)
	}
	if !strings.Contains(rec.Key, "replicas=2") {
		t.Errorf("record key = %q, want the edited call", rec.Key)
	}
	wantRevertsTo(t, rec, 3)
}

func TestCapture_OnTheGrantedPath(t *testing.T) {
	// The call nobody was asked about. It executes on the strength of an
	// approval given for a different call in the same set, so if any
	// path were going to be missing the door it would be this one.
	caps := newGateCaptures(t)
	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 1))
	p := runChangeSetProbe(t, csConfig{
		set:      fixtureChangeSet(),
		scripts:  scripts,
		turns:    turns,
		captures: caps.rules,
	})

	if len(p.executions) != 2 {
		t.Fatalf("tool executed %d time(s), want 2: %+v", len(p.executions), p.executions)
	}
	recs := captureRecords(t, p.state)
	if len(recs) != 2 {
		t.Fatalf("capture records = %d, want one per executed call: %+v", len(recs), recs)
	}
	var granted bool
	for _, r := range recs {
		if strings.Contains(r.Key, "worker") {
			granted = true
			wantRevertsTo(t, r, 3)
		}
	}
	if !granted {
		keys := make([]string, 0, len(recs))
		for _, r := range recs {
			keys = append(keys, r.Key)
		}
		t.Errorf("no capture for the granted call; records name %v", keys)
	}
}

// --- fail-closed ----------------------------------------------------

func TestCaptureFailure_RefusesTheCallOnEveryPath(t *testing.T) {
	// Declaring a capture is an operator saying "do not change this
	// without recording what it was". The honest way to honour that when
	// the recording fails is to not change it — and it costs nothing,
	// because the whole capture happens before the forward call.
	paths := []struct {
		name string
		cfg  gateProbeConfig
	}{
		{"apply", gateProbeConfig{policy: OnMutationApply}},
		{"approved", gateProbeConfig{
			policy:  OnMutationRequireApproval,
			respond: approve(map[string]any{"verdict": "approve", "approver": "user:sre-oncall"}),
		}},
		{"edited", gateProbeConfig{
			policy:  OnMutationRequireApproval,
			respond: approve(editVerdict(map[string]any{"deployment": "api", "replicas": 2})),
		}},
	}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			caps := newGateCaptures(t)
			caps.err = errors.New("the cluster API is unreachable")
			cfg := tc.cfg
			cfg.captures = caps.rules
			p := runGateProbe(t, cfg)

			if len(p.executions) != 0 {
				t.Fatalf("tool executed %+v despite the capture failing; the change happened with no record of what it overwrote", p.executions)
			}
			if len(captureRecords(t, p.state)) != 0 {
				t.Error("a failed capture still wrote a record")
			}
			resp := p.lastResponse(t)
			wantField(t, resp, "error", "capture_failed")
			wantDetailMentions(t, resp, "unreachable")
		})
	}
}

func TestCaptureFailure_RefusesTheGrantedCallAndLeavesTheGrantUnspent(t *testing.T) {
	// Ordering claim from grantgate.go: the capture runs before the
	// grant is marked consumed. The operator authorized this call and
	// nothing has happened yet; burning their answer on a read that
	// failed would make them approve the whole set again to get back to
	// where they already were.
	caps := newGateCaptures(t)
	failing := caps.rules.Read
	seen := 0
	caps.rules.Read = func(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error) {
		seen++
		if seen > 1 {
			// The first call is the directly-approved one; fail only the
			// granted call so the grant exists to be examined.
			return nil, errors.New("the cluster API is unreachable")
		}
		return failing(ctx, name, args)
	}

	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 1))
	p := runChangeSetProbe(t, csConfig{
		set:      fixtureChangeSet(),
		scripts:  scripts,
		turns:    turns,
		captures: caps.rules,
	})

	if !p.executed("api", 3) {
		t.Fatalf("the directly-approved call did not run: %+v", p.executions)
	}
	if p.executed("worker", 1) {
		t.Fatalf("the granted call ran despite its capture failing: %+v", p.executions)
	}
	for _, g := range p.grants(t) {
		if g.ConsumedBy != "" {
			t.Errorf("grant %q was spent on a call that never ran; the operator would have to approve the set again", g.Signature)
		}
	}
}

func TestUndeclaredCaptureLeavesTheCallUntouched(t *testing.T) {
	// A tool that declares no capture behaves exactly as it did before
	// #296: no read, no record, no refusal. This is what makes the
	// feature free for every workload that has not adopted it.
	reads := 0
	rules := &CaptureRules{
		For: func(string) (*Capture, error) { return nil, nil },
		Read: func(adkagent.Context, string, map[string]any) (map[string]any, error) {
			reads++
			return priorState(), nil
		},
	}
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationApply, captures: rules})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
	if reads != 0 {
		t.Errorf("capture read ran %d time(s) for a tool that declares none", reads)
	}
	if got := captureRecords(t, p.state); len(got) != 0 {
		t.Errorf("records written for a tool that declares no capture: %+v", got)
	}
}

func TestUndeclarableCaptureRefusesRatherThanSkips(t *testing.T) {
	// A declaration lookup that errors is not "no declaration". mast
	// does not know whether this tool wanted recording, and guessing
	// "no" is the guess that loses the record.
	rules := &CaptureRules{
		For: func(string) (*Capture, error) { return nil, errors.New("bundle is unreadable") },
	}
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationApply, captures: rules})

	if len(p.executions) != 0 {
		t.Fatalf("tool executed %+v despite an undeclarable capture", p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "capture_failed")
	wantDetailMentions(t, p.lastResponse(t), "unreadable")
}

func TestCaptureRecordCarriesItsProvenance(t *testing.T) {
	// The record has to be legible without the session it came from —
	// same argument as Decision's workload stamp (v0.4 W8). The function
	// call id matters most: it is the join the effects outbox pairs a
	// durable call against its response on, which is what lets an
	// operator tell "this may have happened" and "here is what to put
	// back" apart.
	caps := newGateCaptures(t)
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationApply, captures: caps.rules})

	rec := wantOneCapture(t, p.state)
	if rec.FunctionCallID == "" {
		t.Error("record carries no function call id, so nothing joins it to the outbox")
	}
	if rec.Session != sid {
		t.Errorf("record session = %q, want %q", rec.Session, sid)
	}
	if rec.Specialist == "" {
		t.Error("record does not name the agent that made the call")
	}
	if rec.Read != "get_deployment" || len(rec.ReadArgs) == 0 {
		t.Errorf("record does not name the read that produced it: read=%q args=%v", rec.Read, rec.ReadArgs)
	}
	if rec.CapturedAt.IsZero() {
		t.Error("record has no capture time")
	}
	if got := DescribeCapture(rec); !strings.Contains(got, "undo with") {
		t.Errorf("description does not offer the undo: %s", got)
	}
}
