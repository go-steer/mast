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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
)

// Prior-state capture: what the world looked like immediately before
// mast changed it, and the call that puts it back (#296).
//
// Everything else in this package is about whether a change should
// happen. This file is about the question an operator asks straight
// afterwards, which mast had no answer to at all: *how do I undo this.*
// mast could propose a change, park it, adjudicate it, execute it and
// record that it executed — and then the only route back was a human
// reconstructing the old state from memory, during an incident, under
// time pressure.
//
// Three rules shape it, and each one is a thing mast cannot do rather
// than a preference:
//
//   - **mast cannot derive the read.** It is deliberately
//     Kubernetes-agnostic and an MCP tool's arguments are opaque to it,
//     so "re-read the object this call is about" is not something it can
//     synthesize. The bundle knows which tool reads the object and which
//     argument names it, so the bundle says — the identical argument
//     Precondition makes, answered the identical way.
//
//   - **mast cannot derive the inverse.** Putting a captured value back
//     is a call, and which call it is depends on the tool. So the bundle
//     declares that too, as a mapping from the capture into the revert
//     call's arguments. What mast contributes is that the mapping is
//     evaluated against real captured state and the result is checked
//     against the real tool's real input schema, so the revert path in
//     the record is a call that could actually run — not prose.
//
//   - **mast does not fire the inverse.** A revert executed by mast on
//     its own initiative is a mutating call no operator approved and no
//     model proposed, arriving from a failure handler rather than from a
//     credential — the one shape the write gate has no fence for. So the
//     recorded revert is a ProposedChange, which is the form this repo
//     already has an approval path for: it goes back through this gate
//     like any other change. mast hands you the undo. A person still
//     says yes to it.
//
// The whole of it happens BEFORE the forward call fires, which is what
// makes the failure mode simple: everything here is fail-closed. A tool
// that declares a capture and whose capture cannot be taken does not
// run. Declaring a capture is an operator saying "do not change this
// without recording what it was", and the honest way to honor that is
// to not change it. A tool that declares none behaves exactly as it did
// before this file existed.
//
// Not covered, and named rather than implied: a specialist dispatched
// by the planner runs on its own runner, so its mutating calls never
// reach this gate and are not captured either (DESIGN.md, and #235).
// That boundary is settled; capture inherits it rather than crossing
// it.

// CaptureStateKeyPrefix namespaces the durable prior-state record of one
// mutating call.
const CaptureStateKeyPrefix = "mast_effect_capture_"

// CaptureStateKey is the state key holding the capture for one call.
//
// Keyed by function call id, the same key the effects outbox pairs a
// durable FunctionCall against its FunctionResponse on. That is the
// useful join: the outbox can tell an operator an effect from an
// interrupted turn may or may not have happened, and this record tells
// them what to put back if it did.
func CaptureStateKey(functionCallID string) string {
	return CaptureStateKeyPrefix + functionCallID
}

// Capture is a workload's declaration of how to record what a mutating
// call is about to change.
type Capture struct {
	// Read names a read-only tool in the same catalog. Refusing a
	// mutating one is the caller's job (internal/compose does it), for
	// the same reason a mutating freshness check is refused: a recording
	// that changes the thing it records is not a recording.
	Read string `json:"read"`

	// Args are literal arguments for the read.
	Args map[string]any `json:"args,omitempty"`

	// ArgsFrom maps a read argument name to the *change's* argument to
	// take it from, so one declaration covers every call to the tool.
	// Same shape and same semantics as Precondition.ArgsFrom.
	ArgsFrom map[string]string `json:"args_from,omitempty"`

	// Fields are dot-separated paths into the read's result. Empty
	// records the whole result, which is the right default here and the
	// wrong one for a precondition: a precondition is a comparison and
	// wants the narrowest read that settles it, while a capture is a
	// reconstruction and wants everything it might need. Declare fields
	// when the read is broad and only part of it is restorable.
	Fields []string `json:"fields,omitempty"`

	// Revert declares the call that puts the captured state back, or nil
	// when the workload has no inverse to offer. Nil is honest and it is
	// not the same as nothing: the record still carries the prior state,
	// and an operator reading it knows what the value was even though
	// mast cannot name the call that restores it.
	Revert *Revert `json:"revert,omitempty"`
}

// Revert declares the call that undoes a change, in terms of the change
// and the state captured before it.
type Revert struct {
	// Call names the tool that performs the undo. Usually the same tool
	// as the change — putting a spec field back is the same patch verb
	// that moved it — but it does not have to be, and it is written out
	// rather than defaulted so the record is readable without knowing
	// which tool it belonged to.
	Call string `json:"call"`

	// Args are literal arguments for the revert call.
	Args map[string]any `json:"args,omitempty"`

	// ArgsFromChange maps a revert argument name to the *change's*
	// argument to take it from — how the revert addresses the same
	// object the change addressed.
	ArgsFromChange map[string]string `json:"args_from_change,omitempty"`

	// ArgsFromCapture maps a revert argument name to a dot-separated
	// path into the *captured read result* — the old value, going back.
	//
	// This is the one genuinely new mechanism here and it is what makes
	// the record a revert path rather than a revert-shaped blank: the
	// arguments are the state as it actually was, resolved at capture
	// time from a real read, not a placeholder for a human to fill in.
	ArgsFromCapture map[string]string `json:"args_from_capture,omitempty"`
}

// CaptureRecord is the durable answer to "how do I undo this": one
// mutating call, the state it was about to overwrite, and the call that
// puts that state back.
type CaptureRecord struct {
	// Tool, Key and Arguments are the change AS IT WILL RUN. On an
	// edited approval that is the operator's arguments, not the model's:
	// a record of what was overwritten has to be a record of the call
	// that overwrote it.
	Tool       string         `json:"tool"`
	Key        string         `json:"key"`
	Arguments  map[string]any `json:"arguments"`
	CapturedAt time.Time      `json:"captured_at"`

	// Read and ReadArgs are the read that produced Prior, recorded so a
	// reader can take it again and compare.
	Read     string         `json:"read"`
	ReadArgs map[string]any `json:"read_args,omitempty"`

	// Prior is the captured state: the whole read result, or — when the
	// declaration narrowed it — a map keyed by the declared paths.
	// PriorFields is that list of paths, and is empty in the first case.
	//
	// Values are kept as values rather than rendered to strings, which
	// is where this parts company with PreconditionSnapshot: that one is
	// comparing and only needs to tell same from different, while this
	// one is the thing that goes back into a call.
	Prior       map[string]any `json:"prior"`
	PriorFields []string       `json:"prior_fields,omitempty"`

	// Digest hashes the whole read result, always, including when Prior
	// is narrowed. Two captures of the same object at different moments
	// are told apart by it.
	Digest string `json:"digest"`

	// Revert is the call that restores Prior, already checked against
	// the revert tool's declared input schema, or nil when the workload
	// declared no inverse.
	//
	// A ProposedChange rather than a bespoke type, because that is the
	// form an operator can already be handed and can already approve:
	// running it means putting it through this same write gate.
	Revert *ProposedChange `json:"revert,omitempty"`

	Workload       string `json:"workload,omitempty"`
	Specialist     string `json:"specialist,omitempty"`
	Session        string `json:"session,omitempty"`
	Invocation     string `json:"invocation,omitempty"`
	FunctionCallID string `json:"function_call_id,omitempty"`
}

// Undoable reports whether this record names a call that would restore
// the captured state.
func (r CaptureRecord) Undoable() bool { return r.Revert != nil }

// EncodeCapture renders a capture for durable state, as a JSON string,
// for the same reason every other record in this package is one: it
// survives every session backend's state encoding unchanged.
func EncodeCapture(r CaptureRecord) (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("approval: encoding prior-state capture: %w", err)
	}
	return string(raw), nil
}

// DecodeCapture parses a record written under a CaptureStateKey. The
// value is whatever the session backend handed back — the JSON string
// this package wrote, or the map a round trip left behind.
func DecodeCapture(v any) (CaptureRecord, error) {
	var raw []byte
	switch t := v.(type) {
	case nil:
		return CaptureRecord{}, fmt.Errorf("approval: no capture record")
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return CaptureRecord{}, fmt.Errorf("approval: re-marshalling capture record: %w", err)
		}
		raw = b
	}
	var r CaptureRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return CaptureRecord{}, fmt.Errorf("approval: capture record is not one: %w", err)
	}
	return r, nil
}

// CaptureRules is the runtime half of prior-state capture: what each
// tool declares, and the two things mast needs to honor a declaration.
type CaptureRules struct {
	// For returns the capture declaration for a tool, or nil if it
	// declares none. An error is fail-closed — see the file comment.
	For func(toolName string) (*Capture, error)

	// Read runs a read-only tool on mast's own behalf. Required when any
	// tool declares a capture; this is the same seam the change-set
	// precondition read uses, and it is the same seam on purpose.
	Read func(ctx agent.Context, toolName string, args map[string]any) (map[string]any, error)

	// Schema resolves a tool's declared input schema, so a computed
	// revert call is checked against the tool that would run it before
	// it is written down. Required when any tool declares a revert:
	// without it mast would be recording an undo it has no reason to
	// believe is callable, which is the kind of record that is worse
	// than none.
	Schema func(toolName string) (*jsonschema.Schema, error)

	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time
}

func (c *CaptureRules) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// declared returns the capture declaration for a tool, or nil.
func (c *CaptureRules) declared(toolName string) (*Capture, error) {
	if c == nil || c.For == nil {
		return nil, nil
	}
	return c.For(toolName)
}

// take runs the capture for one about-to-fire call and builds the
// record, including the revert path when the declaration names one.
//
// It is called with the arguments that will actually run. Any error is
// the caller's cue to refuse the call: nothing has happened yet, so
// refusing costs an incident one re-proposal and buys the guarantee the
// declaration asked for.
func (c *CaptureRules) take(ctx agent.Context, toolName, key string, args map[string]any, decl *Capture) (*CaptureRecord, error) {
	if decl == nil {
		return nil, nil
	}
	readArgs, err := decl.readArgs(toolName, args)
	if err != nil {
		return nil, err
	}
	if c.Read == nil {
		return nil, fmt.Errorf("tool %q declares a prior-state capture (read %q) but this deployment cannot run a read on its own behalf", toolName, decl.Read)
	}
	result, err := c.Read(ctx, decl.Read, readArgs)
	if err != nil {
		return nil, fmt.Errorf("capture read %s failed: %w", CallKey(decl.Read, readArgs), err)
	}
	if result == nil {
		// Not the same as an empty object, and the difference matters: a
		// read that returned nothing at all has told mast nothing about
		// the object, and a capture of nothing restores nothing.
		return nil, fmt.Errorf("capture read %s returned no result, so there is nothing recorded to go back to", CallKey(decl.Read, readArgs))
	}
	prior, err := decl.prior(result)
	if err != nil {
		return nil, fmt.Errorf("capture read %s: %w", CallKey(decl.Read, readArgs), err)
	}
	rec := &CaptureRecord{
		Tool:        toolName,
		Key:         key,
		Arguments:   copyArgs(args),
		CapturedAt:  c.now().UTC(),
		Read:        decl.Read,
		ReadArgs:    readArgs,
		Prior:       prior,
		PriorFields: append([]string(nil), decl.Fields...),
		Digest:      resultDigest(result),
	}
	if ctx != nil {
		rec.Specialist = ctx.AgentName()
		rec.Session = ctx.SessionID()
		rec.Invocation = ctx.InvocationID()
		rec.FunctionCallID = ctx.FunctionCallID()
	}
	if decl.Revert != nil {
		ch, err := c.revert(toolName, args, result, decl.Revert)
		if err != nil {
			return nil, err
		}
		rec.Revert = ch
	}
	return rec, nil
}

// revert builds and validates the call that puts the captured state
// back.
func (c *CaptureRules) revert(toolName string, args, result map[string]any, r *Revert) (*ProposedChange, error) {
	call := strings.TrimSpace(r.Call)
	if call == "" {
		return nil, fmt.Errorf("tool %q declares a revert with no call", toolName)
	}
	out := make(map[string]any, len(r.Args)+len(r.ArgsFromChange)+len(r.ArgsFromCapture))
	for k, v := range r.Args {
		out[k] = v
	}
	for revertArg, changeArg := range r.ArgsFromChange {
		v, ok := args[changeArg]
		if !ok {
			return nil, fmt.Errorf("tool %q's revert takes %s from the call's %q argument, which this call does not carry", toolName, revertArg, changeArg)
		}
		out[revertArg] = v
	}
	for revertArg, path := range r.ArgsFromCapture {
		v, ok := lookupPath(result, path)
		if !ok {
			// Fail closed, the same way a missing precondition field
			// does. A revert argument taken from a path the read does not
			// return is a revert that would put something else back.
			return nil, fmt.Errorf("tool %q's revert takes %s from the captured %q, which the capture read did not return", toolName, revertArg, path)
		}
		out[revertArg] = v
	}
	if c.Schema == nil {
		return nil, fmt.Errorf("tool %q declares a revert but this deployment cannot look up %q's arguments, so mast cannot tell whether the revert it built is callable", toolName, call)
	}
	schema, err := c.Schema(call)
	if err != nil {
		return nil, fmt.Errorf("tool %q's revert calls %q: %w", toolName, call, err)
	}
	norm, err := NormalizeArgs(call, schema, out)
	if err != nil {
		return nil, fmt.Errorf("tool %q's revert would call %s, which %q will not accept: %w", toolName, CallKey(call, out), call, err)
	}
	return &ProposedChange{Tool: call, Arguments: norm}, nil
}

// readArgs builds the capture read's arguments for one call.
func (c Capture) readArgs(toolName string, args map[string]any) (map[string]any, error) {
	if strings.TrimSpace(c.Read) == "" {
		return nil, fmt.Errorf("tool %q declares a prior-state capture with no read tool", toolName)
	}
	out := make(map[string]any, len(c.Args)+len(c.ArgsFrom))
	for k, v := range c.Args {
		out[k] = v
	}
	for readArg, changeArg := range c.ArgsFrom {
		v, ok := args[changeArg]
		if !ok {
			return nil, fmt.Errorf("tool %q's capture takes %s from the call's %q argument, which this call does not carry", toolName, readArg, changeArg)
		}
		out[readArg] = v
	}
	return out, nil
}

// prior selects what the record keeps out of the read's result.
func (c Capture) prior(result map[string]any) (map[string]any, error) {
	if len(c.Fields) == 0 {
		return result, nil
	}
	out := make(map[string]any, len(c.Fields))
	for _, path := range c.Fields {
		v, ok := lookupPath(result, path)
		if !ok {
			return nil, fmt.Errorf("declared field %q is not in the result, so the capture would record nothing where it promised the old value", path)
		}
		out[path] = v
	}
	return out, nil
}

// resultDigest hashes a read result over its JSON encoding, whose object
// keys are sorted at every depth, so two results differing only in map
// iteration order hash the same.
func resultDigest(result map[string]any) string {
	raw, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// DescribeCapture renders a capture record as the two lines an operator
// needs: what was overwritten, and the call that puts it back.
func DescribeCapture(r CaptureRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s overwrote state read by %s", r.Key, CallKey(r.Read, r.ReadArgs))
	if len(r.PriorFields) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(r.PriorFields, ", "))
	}
	if r.Revert == nil {
		b.WriteString("; this workload declares no call that puts it back")
		return b.String()
	}
	fmt.Fprintf(&b, "; undo with %s", CallKey(r.Revert.Tool, r.Revert.Arguments))
	return b.String()
}
