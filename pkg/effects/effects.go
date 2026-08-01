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

// Package effects implements the recorded-effect outbox
// (docs/durable-execution-design.md, "Recorded-effect outbox"): the
// runtime guard that makes mutating-tool re-execution ambiguity visible
// and blocking instead of silent, under mast's declared at-least-once
// contract.
//
// The session event log IS the outbox — a durable FunctionCall event is
// the intent record, its paired FunctionResponse the completion record,
// both keyed by (session, invocation, function-call ID). This package
// adds no storage; it only reads history and refuses or replays calls.
//
// The guard ships as an ADK runner plugin so it sits at the one seam
// every tool execution crosses (Flow.callTool wraps MCP, builtin, and
// federation tools alike) and cannot be missed by a new construction
// path — the same lesson as the SQLite write hardening (#53). Both
// runner construction sites (the daemon and the library root) attach
// the same plugin; the permission gate's runtime wiring will share this
// layer, with the outbox check running first (a replayed result
// performs no new effect and needs no fresh approval).
package effects

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// Class is a tool's effect classification under the mutation predicate
// (docs/orchestration-design.md, hitl_policy.on_mutation).
type Class int

const (
	// ClassReadOnly tools never pay the outbox check.
	ClassReadOnly Class = iota

	// ClassMutating tools get intent/completion records in the log and
	// are refused in ambiguous-effect mode. Default for unknown tools:
	// MCP annotations are advisory and ADK v2.1.0's mcptoolset drops
	// them entirely (convertTool copies name/description/schemas only),
	// so default-deny-unknown is both the designed and the only
	// implementable stance; operators un-gate known-safe tools via the
	// workload tool_catalog override.
	ClassMutating

	// ClassSpawning tools start sub-runs whose inner tool calls this
	// process cannot individually guard (the planner dispatch runner is
	// a separate in-memory-session runner; run_shape_* likewise when
	// implemented). They carry no records of their own but are refused
	// in ambiguous-effect mode — fail-closed: an interrupted session
	// must not smuggle mutations through a sub-run while its prior
	// effects are unaccounted for.
	ClassSpawning
)

// controlCalls are engine control-flow function calls, never tool
// effects: dangling ones are the normal wire shape of a paused session
// (a pending RequestInput IS an unpaired FunctionCall) and must not
// trip ambiguous-effect mode.
var controlCalls = map[string]bool{
	"adk_request_input":               true,
	"adk_request_credential":          true,
	toolconfirmation.FunctionCallName: true, // "adk_request_confirmation"
	"finish_task":                     true, // Task-mode completion signal
}

// builtinClasses classifies mast's own registered tools. Names are
// string literals to keep this package dependency-light; a test
// cross-checks them against the pkg/planner and pkg/federation
// constants so drift fails CI.
var builtinClasses = map[string]Class{
	"invoke_specialist":        ClassSpawning,
	"run_shape_llm_router":     ClassSpawning,
	"run_shape_fan_out_fan_in": ClassSpawning,
	"invoke_remote_agent":      ClassMutating, // remote effects are invisible to this process
	"request_operator_input":   ClassReadOnly, // escalation/pause control surface
}

// Predicate classifies a tool by name. See NewPredicate.
type Predicate func(toolName string) Class

// NewPredicate builds the mutation predicate: control surfaces are
// read-only, mast builtins use their registered class, per-tool
// overrides from the workload bundle apply next, and everything else —
// MCP tools included — defaults to mutating (default-deny-unknown).
// Overrides are audit-logged at construction by the caller (pkg/effects
// has no logger of its own at predicate-build time).
func NewPredicate(overrides map[string]bool) Predicate {
	return func(name string) Class {
		if controlCalls[name] {
			return ClassReadOnly
		}
		if mutating, ok := overrides[name]; ok {
			if mutating {
				return ClassMutating
			}
			return ClassReadOnly
		}
		if c, ok := builtinClasses[name]; ok {
			return c
		}
		return ClassMutating
	}
}

// DanglingIntent is a durable mutating (or spawning) FunctionCall from
// a prior attempt with no completion in the log — the call may or may
// not have executed, and the window between an external effect
// committing and its completion event persisting cannot be closed,
// only detected.
type DanglingIntent struct {
	ToolName     string
	CallID       string
	InvocationID string
	Timestamp    time.Time
}

// Config configures the outbox plugin.
type Config struct {
	// Predicate classifies tools; required.
	Predicate Predicate

	// AckedAt returns the operator's effects-acknowledgement watermark
	// for a session, if one exists (pkg/transcript reads it from the
	// companion ops row). Dangling intents at or before the watermark
	// are considered operator-acknowledged and do not trip
	// ambiguous-effect mode. Optional; nil means no acks.
	AckedAt func(ctx context.Context, sessionID string) (time.Time, bool)

	// Logger for refusals, replays, and mode transitions. Optional.
	Logger *slog.Logger
}

// New builds the outbox as an ADK runner plugin. Attach it via
// runner.Config.PluginConfig at every runner construction site.
func New(cfg Config) (*plugin.Plugin, error) {
	if cfg.Predicate == nil {
		return nil, fmt.Errorf("effects: Config.Predicate is required")
	}
	o := &outbox{
		cfg:   cfg,
		modes: make(map[string][]DanglingIntent),
	}
	if o.cfg.Logger == nil {
		o.cfg.Logger = slog.Default()
	}
	return plugin.New(plugin.Config{
		Name:               "mast-effects-outbox",
		BeforeRunCallback:  o.beforeRun,
		BeforeToolCallback: o.beforeTool,
		AfterRunCallback:   o.afterRun,
	})
}

// outbox holds the per-invocation ambiguous-effect mode state. Keyed by
// invocation ID: resume turns reuse the paused run's invocation ID, and
// the daemon serializes turns per session (#62), so one entry maps to
// one live turn. Entries are removed by afterRun (deferred by the
// runner on every path).
type outbox struct {
	cfg   Config
	mu    sync.Mutex
	modes map[string][]DanglingIntent
}

// beforeRun scans the session history for dangling mutating intents
// from prior attempts. At this point the current turn has made no
// calls, so every unpaired non-control FunctionCall in the log is a
// prior attempt's. Never early-exits the run: non-mutating work in the
// turn proceeds even in ambiguous-effect mode.
func (o *outbox) beforeRun(ictx agent.InvocationContext) (*genai.Content, error) {
	sess := ictx.Session()
	if sess == nil {
		return nil, nil
	}
	dangling := scanDangling(sess.Events(), o.cfg.Predicate)
	if len(dangling) == 0 {
		return nil, nil
	}
	if o.cfg.AckedAt != nil {
		if ack, ok := o.cfg.AckedAt(ictx, sess.ID()); ok {
			kept := dangling[:0]
			for _, d := range dangling {
				if d.Timestamp.After(ack) {
					kept = append(kept, d)
				}
			}
			dangling = kept
		}
	}
	if len(dangling) == 0 {
		return nil, nil
	}
	o.mu.Lock()
	o.modes[ictx.InvocationID()] = dangling
	o.mu.Unlock()
	o.cfg.Logger.Warn("ambiguous prior effect: session has unresolved mutating tool calls from an interrupted turn; refusing mutating calls this turn",
		"session", sess.ID(), "invocation", ictx.InvocationID(),
		"dangling", describe(dangling))
	return nil, nil
}

// beforeTool is the per-call check: replay an already-completed call's
// recorded result, refuse mutating/spawning calls in ambiguous-effect
// mode, and let everything else through. Returning a non-nil map makes
// the runner skip the tool and use the map as its result.
func (o *outbox) beforeTool(ctx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	class := o.cfg.Predicate(t.Name())
	if class == ClassReadOnly {
		return nil, nil
	}

	// Call-level exact-key replay: a durable completion for this exact
	// function-call ID means the effect already happened — return the
	// recorded result instead of re-executing. Belt-and-suspenders: no
	// known ADK path re-fires a recorded call through callTool, but the
	// check is cheap and guards any future path that does.
	if class == ClassMutating {
		if sess := ctx.Session(); sess != nil {
			if resp, ok := recordedCompletion(sess.Events(), ctx.FunctionCallID()); ok {
				o.cfg.Logger.Info("replaying recorded effect instead of re-executing",
					"session", sess.ID(), "tool", t.Name(), "function_call_id", ctx.FunctionCallID())
				return resp, nil
			}
		}
	}

	o.mu.Lock()
	dangling := o.modes[ctx.InvocationID()]
	o.mu.Unlock()
	if len(dangling) == 0 {
		return nil, nil
	}
	o.cfg.Logger.Warn("refusing tool call in ambiguous-effect mode",
		"tool", t.Name(), "function_call_id", ctx.FunctionCallID(),
		"invocation", ctx.InvocationID())
	return map[string]any{
		"error": "ambiguous_prior_effect",
		"detail": "this session has mutating tool calls from an interrupted prior turn whose outcome is unknown; " +
			"mutating and sub-run-spawning calls are refused until an operator acknowledges " +
			"(mast sessions resume --ack-effects, or abort the session)",
		"dangling_calls": describe(dangling),
	}, nil
}

// afterRun drops the invocation's mode entry; the runner defers this on
// every Run path.
func (o *outbox) afterRun(ictx agent.InvocationContext) {
	o.mu.Lock()
	delete(o.modes, ictx.InvocationID())
	o.mu.Unlock()
}

// scanDangling pairs FunctionCall parts with FunctionResponse parts
// across the whole event sequence and returns the unpaired calls that
// classify as mutating or spawning. Long-running calls (pending by
// design — the event marks their IDs) and control calls are excluded.
func scanDangling(events session.Events, pred Predicate) []DanglingIntent {
	open := map[string]DanglingIntent{}
	var order []string
	for ev := range events.All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		longRunning := map[string]bool{}
		for _, id := range ev.LongRunningToolIDs {
			longRunning[id] = true
		}
		for _, part := range ev.Content.Parts {
			if part == nil {
				continue
			}
			if fc := part.FunctionCall; fc != nil {
				if longRunning[fc.ID] || controlCalls[fc.Name] {
					continue
				}
				if c := pred(fc.Name); c != ClassMutating && c != ClassSpawning {
					continue
				}
				if _, seen := open[fc.ID]; !seen {
					order = append(order, fc.ID)
				}
				open[fc.ID] = DanglingIntent{
					ToolName:     fc.Name,
					CallID:       fc.ID,
					InvocationID: ev.InvocationID,
					Timestamp:    ev.Timestamp,
				}
			}
			if fr := part.FunctionResponse; fr != nil {
				delete(open, fr.ID)
			}
		}
	}
	var out []DanglingIntent
	for _, id := range order {
		if d, ok := open[id]; ok {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

// recordedCompletion returns the recorded FunctionResponse payload for
// the given function-call ID, if the log already holds one.
func recordedCompletion(events session.Events, callID string) (map[string]any, bool) {
	if callID == "" {
		return nil, false
	}
	for ev := range events.All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part == nil || part.FunctionResponse == nil {
				continue
			}
			if part.FunctionResponse.ID == callID {
				return part.FunctionResponse.Response, true
			}
		}
	}
	return nil, false
}

// describe renders dangling intents for logs and refusal payloads.
func describe(ds []DanglingIntent) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, fmt.Sprintf("%s (call %s, invocation %s, %s)",
			d.ToolName, d.CallID, d.InvocationID, d.Timestamp.UTC().Format(time.RFC3339)))
	}
	return out
}

// Overrides flattens workload tool_catalog per-tool policies into the
// override map NewPredicate consumes, logging each applied override
// (the audit-logged requirement from the mutation predicate's
// definition). Later entries win on duplicate names; the workload
// loader rejects duplicates so that only matters for hand-built maps.
func Overrides(logger *slog.Logger, policies []ToolPolicy) map[string]bool {
	if len(policies) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	out := make(map[string]bool, len(policies))
	for _, p := range policies {
		name := strings.TrimSpace(p.Name)
		if name == "" || p.Mutating == nil {
			continue
		}
		out[name] = *p.Mutating
		logger.Info("tool mutation-class override applied (workload tool_catalog)",
			"tool", name, "mutating", *p.Mutating)
	}
	return out
}

// ToolPolicy mirrors workload.ToolPolicy without importing pkg/workload
// (which would drag the YAML loader into every library embed that only
// wants the guard). The daemon and library root convert.
type ToolPolicy struct {
	Name     string
	Mutating *bool
}
