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

package workload

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/mast/pkg/monitor"
)

// Load reads a workload bundle YAML file, parses it, validates required
// fields, and returns the populated Bundle.
func Load(path string) (Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, fmt.Errorf("workload: read %q: %w", path, err)
	}
	var b Bundle
	if err := yaml.Unmarshal(data, &b); err != nil {
		return Bundle{}, fmt.Errorf("workload: parse %q: %w", path, err)
	}
	b.Filename = path
	if err := b.foldHITLPolicy(); err != nil {
		return Bundle{}, fmt.Errorf("workload: parse %q: %w", path, err)
	}
	if err := b.validate(); err != nil {
		return Bundle{}, fmt.Errorf("workload: validate %q: %w", path, err)
	}
	if b.Mode == "" {
		b.Mode = ModeSingleSession
	}
	return b, nil
}

// foldHITLPolicy collapses the documented `hitl_policy:` spelling onto
// the shipped `hitl:` field so everything downstream reads one place.
func (b *Bundle) foldHITLPolicy() error {
	if b.HITLPolicy == (HITL{}) {
		return nil
	}
	if b.HITL != (HITL{}) {
		return fmt.Errorf("both hitl: and hitl_policy: are set; they are the same block — keep one")
	}
	b.HITL = b.HITLPolicy
	b.HITLPolicy = HITL{}
	return nil
}

func (b *Bundle) validate() error {
	if b.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(b.Specialists) == 0 {
		return fmt.Errorf("specialists roster is empty (workload cannot dispatch)")
	}
	if b.Mode != "" && b.Mode != ModeSingleSession && b.Mode != ModeMultiSession {
		return fmt.Errorf("unknown mode %q (want single_session or multi_session)", b.Mode)
	}
	switch b.Dispatch {
	case "", DispatchCoordinator, DispatchGraph, DispatchFanout, DispatchBounded, DispatchAuto:
	default:
		return fmt.Errorf("unknown dispatch %q (want coordinator, graph, fanout, bounded, or auto)", b.Dispatch)
	}
	// Validated at load, like the scheduled cadence below, so a typo'd
	// posture is a refused bundle naming the file rather than a
	// backstop that quietly resolves to the default. The empty case is
	// legitimate and distinct from "warn" — it means "leave it to the
	// host", which is what lets --watchdog win.
	switch b.Safety.Watchdog {
	case "", WatchdogWarn, WatchdogFeedback, WatchdogEnforce:
	default:
		return fmt.Errorf("unknown safety.watchdog %q (want %s, %s, or %s)",
			b.Safety.Watchdog, WatchdogWarn, WatchdogFeedback, WatchdogEnforce)
	}
	switch b.HITL.OnMutation {
	case "", OnMutationRequireApproval, OnMutationApply, OnMutationDryRun:
	default:
		return fmt.Errorf("unknown hitl.on_mutation %q (want require_approval, apply, or dry_run)", b.HITL.OnMutation)
	}
	seen := make(map[string]bool, len(b.Specialists))
	for _, name := range b.Specialists {
		if name == "" {
			return fmt.Errorf("specialists roster contains an empty entry")
		}
		if seen[name] {
			return fmt.Errorf("specialists roster contains duplicate %q", name)
		}
		seen[name] = true
	}
	if _, err := b.HITL.EffectiveChangeSetTTL(); err != nil {
		return err
	}
	// The cadence is resolved at load so a typo'd interval is a refused
	// bundle naming the file, not a trigger that quietly never fires.
	// Jitter is resolved too: it is validated against the interval, and
	// the daemon should never be the first thing to discover that the
	// two do not go together.
	if b.EdgeTrigger.Scheduled != nil {
		if _, err := b.EdgeTrigger.Scheduled.EffectiveJitter(); err != nil {
			return err
		}
	}
	if err := b.validateMonitor(); err != nil {
		return err
	}
	seenTools := make(map[string]bool, len(b.ToolCatalog.Tools))
	for _, p := range b.ToolCatalog.Tools {
		if p.Name == "" {
			return fmt.Errorf("tool_catalog.tools contains an entry without a name")
		}
		if seenTools[p.Name] {
			return fmt.Errorf("tool_catalog.tools contains duplicate %q", p.Name)
		}
		seenTools[p.Name] = true
		if err := validatePrecondition(p); err != nil {
			return err
		}
		if err := validateCapture(p); err != nil {
			return err
		}
	}
	return nil
}

// validateMonitor checks what the bundle alone can check about the
// collection leg (v0.5 W4.2).
//
// Whether a named tool is wired at all is checked at the moment it is
// used, where the toolsets are — the same split validatePrecondition
// makes, and for the same reason: enumerating a live toolset means
// connecting to every MCP server, which is not something a YAML parse
// should do. Whether a named tool has leaked into a specialist's reach
// is checked at composition, where the roster is.
func (b *Bundle) validateMonitor() error {
	if err := b.validateMonitorNotify(); err != nil {
		return err
	}
	if err := b.validateMonitorAck(); err != nil {
		return err
	}
	if !b.Monitor.Enabled() {
		return nil
	}
	// A collection with nothing to trigger it is a block an operator
	// believes is running and that has never once run. Refuse it here
	// rather than let the daemon come up quietly doing nothing — the
	// same argument EffectiveInterval makes about a cadence that does
	// not parse.
	if b.EdgeTrigger.Scheduled == nil {
		return fmt.Errorf("monitor.collect names %d call(s) but the workload declares no edge_trigger.scheduled block; the collection leg runs at the top of a scheduled cycle, so without a cadence it never runs at all", len(b.Monitor.Collect))
	}
	seen := make(map[string]bool, len(b.Monitor.Collect))
	for i, c := range b.Monitor.Collect {
		if strings.TrimSpace(c.Tool) == "" {
			return fmt.Errorf("monitor.collect[%d] names no tool", i)
		}
		key := c.Key()
		if seen[key] {
			// Two entries filed under one key means one result silently
			// overwrites the other. The fix depends on which case it is,
			// so the message names both.
			return fmt.Errorf("monitor.collect files two results under %q; give one of them an `as:` key, or drop the duplicate", key)
		}
		seen[key] = true
	}
	// A transitions_from that names nothing collected is a workload
	// that believes it is watching for change and is not. It fails on
	// the first fire either way; failing at load names the typo and
	// lists what was actually collected, which is the difference
	// between a five-second fix and reading a stack trace at 3am.
	if key := b.Monitor.TransitionsKey(); key != "" && !seen[key] {
		collected := make([]string, 0, len(seen))
		for k := range seen {
			collected = append(collected, k)
		}
		sort.Strings(collected)
		return fmt.Errorf("monitor.transitions_from names %q, which no monitor.collect entry files a result under (collected: %s)", key, strings.Join(collected, ", "))
	}
	return nil
}

// validateMonitorNotify checks the egress half (v0.5 W4.5).
//
// Whether the conversation exists, and whether this deployment is even
// allowed to post into it, is switchboard's answer, not mast's — mast
// finds out at the first fire, with a 403 it reports. What mast can
// check here is that the block says where, and that the deadman parses.
func (b *Bundle) validateMonitorNotify() error {
	if b.Monitor.Notify == nil {
		return nil
	}
	if b.Monitor.NotifyTarget() == "" {
		return fmt.Errorf("monitor.notify names no conversation; a cycle that speaks has to say where")
	}
	if _, err := b.Monitor.EffectiveDigestAfter(); err != nil {
		return err
	}
	// Same argument as the collection leg: a notify block on a workload
	// with no cadence is an operator who believes a chat is being kept
	// up to date by something that never runs.
	if b.EdgeTrigger.Scheduled == nil {
		return fmt.Errorf("monitor.notify posts into %q but the workload declares no edge_trigger.scheduled block; a cycle speaks at the end of a scheduled cycle, so without a cadence it never speaks at all", b.Monitor.NotifyTarget())
	}
	return nil
}

// validateMonitorAck checks the inbound half (v0.5 W4.6).
//
// Deliberately does NOT require a cadence, unlike collect and notify.
// An ack does not arrive from a cycle — it arrives from an operator, on
// the daemon's ingress, at whatever hour they read the message. A
// workload can sensibly take acks for findings something else surfaces.
func (b *Bundle) validateMonitorAck() error {
	if b.Monitor.Ack == nil {
		return nil
	}
	if b.Monitor.AckTool() == "" {
		return fmt.Errorf("monitor.ack names no tool; an ack mast cannot forward is a suppression that never happens, silently")
	}
	// The two arguments mast supplies are the two an operator must not
	// be able to pre-empt. subject_key is what the ack is about and
	// comes from the request; ack_by is who asked and comes from their
	// credential. A bundle that pinned either would make every ack read
	// as the same one — which is exactly the attribution failure the
	// whole path is arranged to prevent (#194 settled the same rule for
	// approvals).
	for _, reserved := range []string{monitor.AckSubjectArg, monitor.AckByArg} {
		if _, ok := b.Monitor.Ack.Args[reserved]; ok {
			return fmt.Errorf("monitor.ack.args sets %q, which mast supplies from the request itself; a bundle that pins it makes every ack look like the same one. Drop it and let the ingress fill it in", reserved)
		}
	}
	// A tool that both classifies and suppresses would have the cycle
	// acking findings on its own behalf every fire — the monitor
	// silencing itself, with an operator's name nowhere near it.
	for _, c := range b.Monitor.Collect {
		if strings.TrimSpace(c.Tool) == b.Monitor.AckTool() {
			return fmt.Errorf("monitor.ack forwards to %q, which monitor.collect also runs every cycle; a cycle that acks what it just classified suppresses findings nobody asked to suppress", b.Monitor.AckTool())
		}
	}
	return nil
}

// validatePrecondition checks what the bundle alone can check about a
// change-set freshness declaration (v0.4 W7). Whether the named read is
// classified read-only is checked at composition, where the mutation
// predicate exists; whether it is wired at all is checked at the moment
// it is used, where the toolsets do.
func validatePrecondition(p ToolPolicy) error {
	pre := p.Precondition
	if pre == nil {
		return nil
	}
	if strings.TrimSpace(pre.Read) == "" {
		return fmt.Errorf("tool_catalog.tools[%q].precondition names no read tool", p.Name)
	}
	if pre.Read == p.Name {
		return fmt.Errorf("tool_catalog.tools[%q].precondition reads %q, which is the tool being checked; a change cannot be its own precondition", p.Name, pre.Read)
	}
	for _, f := range pre.Fields {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].precondition.fields contains an empty path", p.Name)
		}
	}
	for readArg, changeArg := range pre.ArgsFrom {
		if strings.TrimSpace(readArg) == "" || strings.TrimSpace(changeArg) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].precondition.args_from maps %q to %q; both sides name an argument", p.Name, readArg, changeArg)
		}
	}
	return nil
}

// validateCapture checks what the bundle alone can check about a
// prior-state capture (#296). The same split validatePrecondition makes:
// read-only classification is composition's job because that is where
// the mutation predicate is, and whether a tool is wired at all is
// checked at the moment it is used.
//
// Everything here is a declaration that cannot mean what it says, and
// every one of them is refused at parse rather than discovered when a
// remediation is halfway through. A capture that silently records
// nothing is worse than no capture: it is a promise of an undo that
// turns out, at the moment it is needed, to be empty.
func validateCapture(p ToolPolicy) error {
	decl := p.Capture
	if decl == nil {
		return nil
	}
	if strings.TrimSpace(decl.Read) == "" {
		return fmt.Errorf("tool_catalog.tools[%q].capture names no read tool", p.Name)
	}
	if decl.Read == p.Name {
		return fmt.Errorf("tool_catalog.tools[%q].capture reads %q, which is the tool being captured; a change cannot record its own prior state", p.Name, decl.Read)
	}
	for _, f := range decl.Fields {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].capture.fields contains an empty path", p.Name)
		}
	}
	for readArg, changeArg := range decl.ArgsFrom {
		if strings.TrimSpace(readArg) == "" || strings.TrimSpace(changeArg) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].capture.args_from maps %q to %q; both sides name an argument", p.Name, readArg, changeArg)
		}
	}
	rev := decl.Revert
	if rev == nil {
		return nil
	}
	if strings.TrimSpace(rev.Call) == "" {
		return fmt.Errorf("tool_catalog.tools[%q].capture.revert names no call", p.Name)
	}
	for revArg, changeArg := range rev.ArgsFromChange {
		if strings.TrimSpace(revArg) == "" || strings.TrimSpace(changeArg) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].capture.revert.args_from_change maps %q to %q; both sides name an argument", p.Name, revArg, changeArg)
		}
	}
	if len(rev.ArgsFromCapture) == 0 && len(rev.Args) == 0 {
		// A revert built only from the change's own arguments puts back
		// whatever the change put in. It is not an undo, and the shape is
		// close enough to a correct one that it would be believed.
		return fmt.Errorf("tool_catalog.tools[%q].capture.revert takes nothing from the capture and names no literal arguments, so it would re-apply the change rather than undo it; map at least one argument through args_from_capture", p.Name)
	}
	// Non-empty fields narrow what the record keeps. A revert argument
	// drawn from outside that narrowing resolves at capture time and is
	// then absent from the record, so a reader looking at the row cannot
	// see where the value came from — and re-deriving it later is
	// impossible. Refuse the disagreement rather than record half of it.
	kept := make(map[string]bool, len(decl.Fields))
	for _, f := range decl.Fields {
		kept[f] = true
	}
	for revArg, path := range rev.ArgsFromCapture {
		if strings.TrimSpace(revArg) == "" || strings.TrimSpace(path) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].capture.revert.args_from_capture maps %q to %q; the left side names an argument and the right side a path into the capture", p.Name, revArg, path)
		}
		if len(kept) > 0 && !kept[path] {
			return fmt.Errorf("tool_catalog.tools[%q].capture.revert takes %s from the captured %q, which capture.fields does not record; add it to fields, or drop fields and record the whole read", p.Name, revArg, path)
		}
	}
	return nil
}
