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
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
)

// The change-set grant: one operator answer, N calls.
//
// W7.0 made a finding carry the exact calls it recommends. This file is
// the other half — what happens when the operator says yes. Without it,
// an approved three-call change set is three separate parks: the
// operator answers the same question three times, having already
// approved the set, and each answer is a fresh chance to approve
// something they did not read. With it, approving the set mints a grant
// per remaining call, bound to that call's exact normalized (tool,
// arguments) signature, and the per-call write gate consumes the grant
// silently instead of parking.
//
// Everything here exists to keep that shortcut from becoming a hole:
//
//   - A grant authorizes ONE signature. Not a tool, not a pattern — the
//     bytes of the call. A model that proposes scale(replicas=2) and
//     then calls scale(replicas=20) finds no grant and parks.
//   - A grant is durable, because the calls it authorizes happen after
//     a resume, possibly after a crash, in a process that does not
//     remember the first pass (docs/spike-findings.md).
//   - A grant is consumed once. Its record carries the function call id
//     that spent it, so a re-dispatch of the same call re-uses it and a
//     second, different call does not.
//   - A grant expires. See Freshness.
//   - A grant can be conditioned on the world not having moved. See
//     Precondition — the part a wall clock cannot do.
//   - A grant is only minted for calls an operator could actually read.
//     See Legible.
//
// Policy is not short-circuited. A granted call still goes through
// permissions.Gate.CheckMutatingToolCall before it runs, because a
// configured deny is maximal and is not something an approval overrides
// — the grant replaces the *question*, never the policy.

// GrantStateKeyPrefix namespaces the durable record of one minted
// change-set grant. One key per authorized call.
//
// Per signature rather than one key per set, because the calls in a set
// are executed independently and each one's consumption has to be
// recorded independently. A single key holding the whole set would make
// two calls in the same turn race to rewrite it, and the loser's
// consumption mark would vanish.
const GrantStateKeyPrefix = "mast_change_grant_"

// GrantStateKey is the state key holding the grant for one call
// signature.
//
// The signature is hashed rather than embedded: it contains the call's
// full arguments, which are arbitrary JSON of arbitrary length, and a
// state key is not the place for a 40KB manifest. The record carries
// the signature in full, and lookup compares it, so a hash collision
// produces "no grant" rather than the wrong grant.
func GrantStateKey(signature string) string {
	sum := sha256.Sum256([]byte(signature))
	return GrantStateKeyPrefix + hex.EncodeToString(sum[:])[:32]
}

// Grant is the durable record of one authorized call.
type Grant struct {
	// Signature is the exact call this grant authorizes, in the form
	// Signature renders. The whole grant hangs off this string being
	// byte-identical to the call that later arrives at the gate.
	Signature string `json:"signature"`

	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`

	// Origin is the call key of the parked call the operator actually
	// answered, and Approver is who answered it. Together they are the
	// audit answer to "who authorized this, and what were they looking
	// at when they did".
	Origin   string `json:"origin"`
	Approver string `json:"approver,omitempty"`
	Note     string `json:"note,omitempty"`

	MintedAt  time.Time `json:"minted_at"`
	ExpiresAt time.Time `json:"expires_at"`

	// Precondition is the snapshot of the world this grant was issued
	// against, or nil when the tool declares no precondition. Re-read
	// before the call fires.
	Precondition *PreconditionSnapshot `json:"precondition,omitempty"`

	// ConsumedBy is the function call id that spent this grant, and
	// VoidedBy is why it can never be spent (a failed freshness check).
	// Both empty on a live grant; a record is rewritten rather than
	// deleted so the audit trail keeps the whole life of the approval.
	ConsumedBy string `json:"consumed_by,omitempty"`
	VoidedBy   string `json:"voided_by,omitempty"`
}

// Spent reports whether this grant can still authorize a call. The
// function call id is the caller's own: ADK re-dispatches a call after
// a confirmation, and a grant spent by *this* call is not spent as far
// as this call is concerned.
func (g Grant) Spent(functionCallID string) bool {
	if g.VoidedBy != "" {
		return true
	}
	return g.ConsumedBy != "" && g.ConsumedBy != functionCallID
}

// EncodeGrant renders a grant for durable state, as a JSON string, for
// the same reason every other record here is one: it survives every
// session backend's state encoding unchanged.
func EncodeGrant(g Grant) (string, error) {
	raw, err := json.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("approval: encoding change-set grant: %w", err)
	}
	return string(raw), nil
}

// DecodeGrant reads a grant back out of durable state.
func DecodeGrant(v any) (Grant, error) {
	var raw []byte
	switch t := v.(type) {
	case nil:
		return Grant{}, fmt.Errorf("approval: no grant record")
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return Grant{}, fmt.Errorf("approval: re-marshalling grant record: %w", err)
		}
		raw = b
	}
	var g Grant
	if err := json.Unmarshal(raw, &g); err != nil {
		return Grant{}, fmt.Errorf("approval: grant record is not one: %w", err)
	}
	return g, nil
}

// Precondition is a workload's declaration of what a change assumed
// about the cluster, expressed as a read this deployment can make.
//
// mast cannot derive this. It is deliberately Kubernetes-agnostic and
// an MCP tool's arguments are opaque to it, so "re-read the object this
// call is about" is not a thing mast can synthesize — it does not know
// which tool reads that object, or which of the write call's arguments
// names it. The bundle knows both, so the bundle says
// (tool_catalog.tools[].precondition).
//
// A tool that declares none gets a TTL and nothing else, which is the
// honest default rather than a safe one: see Freshness.
type Precondition struct {
	// Read names a read-only tool in the same catalog. Refusing a
	// mutating one is the caller's job (internal/compose does it): a
	// freshness check that changes the cluster is not a check.
	Read string `json:"read"`

	// Args are literal arguments for the read.
	Args map[string]any `json:"args,omitempty"`

	// ArgsFrom maps a read argument name to the *change's* argument to
	// take it from, so one declaration covers every call to the tool:
	// {namespace: namespace, name: deployment} turns
	// scale_deployment(deployment=api, namespace=prod, replicas=5) into
	// get_deployment(name=api, namespace=prod).
	//
	// A named argument the change does not carry is an error, not an
	// omission: it means the declaration does not describe this call,
	// and guessing would produce a check against the wrong object.
	ArgsFrom map[string]string `json:"args_from,omitempty"`

	// Fields are dot-separated paths into the read's result, compared
	// individually so an operator is told what moved rather than that
	// something did.
	//
	// Empty means compare the whole result. That is the blunt option
	// and it is the default on purpose: a narrow read
	// (`get_deployment -o jsonpath={.spec.replicas}`) needs no paths,
	// and a broad one that changes on every heartbeat should be
	// narrowed at the source rather than filtered here.
	Fields []string `json:"fields,omitempty"`
}

// PreconditionSnapshot is what the read returned at approval time.
type PreconditionSnapshot struct {
	Read string         `json:"read"`
	Args map[string]any `json:"args,omitempty"`

	// Digest is a hash of the whole result, always recorded. The result
	// itself is not: it is cluster state, it can be large, and a
	// session log is not where an operator expects to find it.
	Digest string `json:"digest"`

	// Fields are the declared paths and their rendered values, which
	// are recorded in full — they are small, and they are the
	// difference between "something changed" and "replicas went 3 → 5".
	Fields map[string]string `json:"fields,omitempty"`
}

// Freshness bounds how long an approval speaks for.
//
// Two clocks matter and neither one is enough alone. Wall time is the
// one mast can always measure: an approval answered from a phone at
// 02:00 and executed when the daemon comes back at 09:00 is not an
// approval of anything anyone looked at. Cluster state is the one that
// actually matters, and it can move in seconds — a change set is even
// self-invalidating by construction, since calls 1..k mutate the world
// calls k+1..N were reasoned about.
//
// So the TTL is a backstop, not the check. Set it long enough that a
// legitimate approve→execute round trip never trips it (a short TTL
// that fires routinely trains operators to re-approve without reading),
// and let the precondition carry the real question. A tool with no
// declared precondition is bounded by the TTL alone, and mast says so
// in the parked question rather than implying a check it is not making.
type Freshness struct {
	// TTL is how long a minted grant lives. Zero means DefaultGrantTTL.
	TTL time.Duration

	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time

	// Precondition returns the freshness declaration for a tool, or
	// nil if it declares none. An error is fail-closed: no grant is
	// minted, and the call parks as it would have before W7.
	Precondition func(toolName string) (*Precondition, error)

	// Read runs a read-only tool and returns its result. Required when
	// any tool declares a precondition; without it a declared
	// precondition cannot be evaluated and no grant is minted.
	Read func(ctx agent.Context, toolName string, args map[string]any) (map[string]any, error)
}

// DefaultGrantTTL is how long a change-set grant lives when the bundle
// does not say.
//
// Ten minutes is chosen against the two failure modes rather than from
// a threat model: it is far longer than the seconds an approved
// executor takes to run its calls, so it never fires during normal
// operation; and it is far shorter than the hours over which an
// operator forgets what they approved. A daemon that crashes and comes
// back inside the window still fires the set, which is the crash
// behaviour W7 is for; one that comes back after it asks again.
const DefaultGrantTTL = 10 * time.Minute

func (f *Freshness) ttl() time.Duration {
	if f == nil || f.TTL <= 0 {
		return DefaultGrantTTL
	}
	return f.TTL
}

func (f *Freshness) now() time.Time {
	if f != nil && f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// snapshot evaluates a change's declared precondition at approval time.
// (nil, nil) means the tool declares none.
func (f *Freshness) snapshot(ctx agent.Context, ch ProposedChange) (*PreconditionSnapshot, error) {
	if f == nil || f.Precondition == nil {
		return nil, nil
	}
	pre, err := f.Precondition(ch.Tool)
	if err != nil {
		return nil, fmt.Errorf("tool %q: %w", ch.Tool, err)
	}
	if pre == nil {
		return nil, nil
	}
	args, err := pre.readArgs(ch)
	if err != nil {
		return nil, err
	}
	if f.Read == nil {
		return nil, fmt.Errorf("tool %q declares a precondition (read %q) but this deployment cannot run a read on its own behalf", ch.Tool, pre.Read)
	}
	result, err := f.Read(ctx, pre.Read, args)
	if err != nil {
		return nil, fmt.Errorf("precondition read %s failed: %w", CallKey(pre.Read, args), err)
	}
	digest, fields, err := digestResult(result, pre.Fields)
	if err != nil {
		return nil, fmt.Errorf("precondition read %s: %w", CallKey(pre.Read, args), err)
	}
	return &PreconditionSnapshot{Read: pre.Read, Args: args, Digest: digest, Fields: fields}, nil
}

// verify re-runs the snapshot's read and reports what moved. An empty
// string means the world still matches; anything else is the reason
// the grant no longer holds, phrased for the operator who will see it
// in the re-parked question.
func (f *Freshness) verify(ctx agent.Context, snap *PreconditionSnapshot, toolName string) string {
	if snap == nil {
		return ""
	}
	if f == nil || f.Read == nil || f.Precondition == nil {
		return fmt.Sprintf("this deployment can no longer evaluate %s's precondition (read %q), so the approval is not being relied on", toolName, snap.Read)
	}
	pre, err := f.Precondition(toolName)
	if err != nil || pre == nil {
		return fmt.Sprintf("%s's precondition declaration is no longer readable, so the approval is not being relied on", toolName)
	}
	result, err := f.Read(ctx, snap.Read, snap.Args)
	if err != nil {
		return fmt.Sprintf("the precondition read %s failed (%v), so mast cannot confirm the cluster still matches what was approved", CallKey(snap.Read, snap.Args), err)
	}
	digest, fields, err := digestResult(result, pre.Fields)
	if err != nil {
		return fmt.Sprintf("the precondition read %s returned something mast could not compare (%v)", CallKey(snap.Read, snap.Args), err)
	}
	if digest == snap.Digest {
		return ""
	}
	// Declared fields turn "it changed" into "this changed". Report
	// every moved field, not the first: an operator re-reading the
	// question needs the whole delta to decide.
	var moved []string
	for _, name := range sortedFieldNames(snap.Fields, fields) {
		was, now := snap.Fields[name], fields[name]
		if was != now {
			moved = append(moved, fmt.Sprintf("%s was %s at approval and is %s now", name, orNone(was), orNone(now)))
		}
	}
	if len(moved) > 0 {
		return fmt.Sprintf("the cluster moved since this was approved: %s (precondition read %s)",
			strings.Join(moved, "; "), CallKey(snap.Read, snap.Args))
	}
	return fmt.Sprintf("the precondition read %s no longer returns what it returned at approval time", CallKey(snap.Read, snap.Args))
}

func orNone(s string) string {
	if s == "" {
		return "absent"
	}
	return s
}

func sortedFieldNames(a, b map[string]string) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readArgs builds the precondition read's arguments for one change.
func (p Precondition) readArgs(ch ProposedChange) (map[string]any, error) {
	if strings.TrimSpace(p.Read) == "" {
		return nil, fmt.Errorf("tool %q declares a precondition with no read tool", ch.Tool)
	}
	args := make(map[string]any, len(p.Args)+len(p.ArgsFrom))
	for k, v := range p.Args {
		args[k] = v
	}
	for readArg, changeArg := range p.ArgsFrom {
		v, ok := ch.Arguments[changeArg]
		if !ok {
			return nil, fmt.Errorf("tool %q's precondition takes %s from the call's %q argument, which this call does not carry", ch.Tool, readArg, changeArg)
		}
		args[readArg] = v
	}
	return args, nil
}

// digestResult hashes a read's result and extracts the declared fields.
//
// The hash is over the JSON encoding, whose object keys are sorted at
// every depth, so two results that differ only in map iteration order
// hash the same — otherwise every check would report drift.
func digestResult(result map[string]any, fields []string) (string, map[string]string, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return "", nil, fmt.Errorf("result is not encodable: %w", err)
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])[:16]
	if len(fields) == 0 {
		return digest, nil, nil
	}
	out := make(map[string]string, len(fields))
	for _, path := range fields {
		v, ok := lookupPath(result, path)
		if !ok {
			// Fail closed. A declared field the read does not return
			// means the declaration and the tool disagree, and a check
			// that silently compares nothing to nothing always passes.
			return "", nil, fmt.Errorf("declared field %q is not in the result", path)
		}
		rendered, err := json.Marshal(v)
		if err != nil {
			return "", nil, fmt.Errorf("declared field %q is not encodable: %w", path, err)
		}
		out[path] = string(rendered)
	}
	return digest, out, nil
}

// lookupPath walks a dot-separated path into a decoded JSON object.
func lookupPath(m map[string]any, path string) (any, bool) {
	cur := any(m)
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Legible reports whether a change set is one an operator can approve
// as a set.
//
// The rule is derived, not declared: every argument of every call must
// render into the call key in full. CallKey elides values over
// maxValueLen because a 40KB manifest in a log line helps nobody — but
// an elided value in the question is a value the operator did not read,
// and minting grants from it would convert "yes to what I can see" into
// "yes to what I cannot". A set with one of those is still approvable
// call by call, where the full arguments travel in each parked
// confirmation's payload and the operator can inspect them one at a
// time. This is the legibility rule of docs/v0.3-plan.md W7: narrow
// named tools, not manifest blobs.
func Legible(changes []ProposedChange) error {
	for i, ch := range changes {
		keys := make([]string, 0, len(ch.Arguments))
		for k := range ch.Arguments {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if elided(ch.Arguments[k]) {
				return fmt.Errorf("%s[%d] calls %s with a %q argument too large to show in the approval question", ChangeSetField, i, ch.Tool, k)
			}
		}
	}
	return nil
}

// elided reports whether CallKey would truncate this value.
func elided(v any) bool {
	if s, ok := v.(string); ok {
		return len(s) > maxValueLen
	}
	b, err := json.Marshal(v)
	if err != nil {
		return true
	}
	return len(b) > maxValueLen
}
