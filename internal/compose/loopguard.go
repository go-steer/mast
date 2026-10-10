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
	"log/slog"
	"maps"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/mast/internal/watchdog"
	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/workload"
)

// LoopGuardPluginName is the in-turn loop guard's plugin name.
const LoopGuardPluginName = "mast-loop-guard"

// DefaultMaxModelCallsPerTurn is the per-turn model-call cap when a
// bundle declares none (#519). Measured on the judged corpus across
// every model tried in core-models' parity runs (2026-10-09): the most
// tool calls any non-looping incident made was 35, and every runaway
// row made 99 or more, up to 221. 100 is about three times the healthy
// worst case and below every runaway. A workload whose honest turns run
// longer — a wide coordinator, say — raises it in its bundle.
const DefaultMaxModelCallsPerTurn = 100

// MaxModelCallsPerTurn resolves a bundle's budget.max_model_calls_per_turn:
// 0 or no bundle is DefaultMaxModelCallsPerTurn, negative is uncapped
// (returned as 0).
func MaxModelCallsPerTurn(b *workload.Bundle) int {
	if b == nil || b.Budget.MaxModelCallsPerTurn == 0 {
		return DefaultMaxModelCallsPerTurn
	}
	if b.Budget.MaxModelCallsPerTurn < 0 {
		return 0
	}
	return b.Budget.MaxModelCallsPerTurn
}

// LoopGuardConfig configures LoopGuard.
type LoopGuardConfig struct {
	// Mode is the watchdog posture. The repeated-call note and stop run
	// only where it feeds (feedback and enforce).
	Mode watchdog.Mode
	// MaxModelCallsPerTurn caps one turn's model calls, under every
	// posture: it is a budget, not a watchdog reaction. 0 is uncapped;
	// resolve a bundle's value through MaxModelCallsPerTurn.
	MaxModelCallsPerTurn int
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// LoopGuard builds the in-turn guard as an ADK runner plugin, or
// returns nil when it has nothing to do — a posture that does not feed
// and no call cap. Callers append it only when non-nil.
//
// Two jobs, one per-turn ledger (watchdog.TurnLoop):
//
//   - The in-turn half of feedback mode (#514). The watchdog's feedback
//     reaches the model on the session's next turn; this reaches it
//     inside the turn, on the result of the repeated call itself, and
//     ends the turn if the model keeps repeating after reading it.
//   - The per-turn model-call cap (#519), the backstop for a runaway no
//     detector recognizes. It refuses the call over the cap before it
//     is made.
//
// Either stop goes through the turn's approval.TurnStop, the handle the
// write gate's refusal loop already uses: an error returned from a
// callback cannot end an ADK turn (#449), so ending one means
// cancelling its context. Without a TurnStop on the context the guard
// still refuses; it just cannot end the turn.
//
// Register it LAST at every runner construction site. ADK stops at the
// first before-tool callback that answers, so a call the outbox replays
// or the write gate disposes of never reaches the guard — those calls
// did not run, and the gate already scrubs them from the watchdog's
// books for the same reason. And the first after-tool callback to
// return a result wins, so registered ahead of a plugin with an
// after-tool hook, the guard would hide results from it.
func LoopGuard(cfg LoopGuardConfig) (*plugin.Plugin, error) {
	repeats := cfg.Mode.Feeds()
	if !repeats && cfg.MaxModelCallsPerTurn <= 0 {
		return nil, nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	g := &loopGuard{
		loop:   watchdog.NewTurnLoop(watchdog.DefaultRepeatThreshold, watchdog.DefaultStopAfterNote),
		logger: logger,
	}
	g.loop.SetMaxModelCalls(cfg.MaxModelCallsPerTurn)
	pc := plugin.Config{
		Name:             LoopGuardPluginName,
		AfterRunCallback: func(ictx adkagent.InvocationContext) { g.loop.Forget(ictx.InvocationID()) },
	}
	if cfg.MaxModelCallsPerTurn > 0 {
		pc.BeforeModelCallback = g.beforeModel
	}
	if repeats {
		pc.BeforeToolCallback = g.beforeTool
		pc.AfterToolCallback = g.afterTool
	}
	return plugin.New(pc)
}

// The refusal the guard answers a stopped call with, as ADK's
// convention spells a tool error.
const (
	loopStoppedKey = "error"
	loopStopped    = "loop_stopped"
)

type loopGuard struct {
	loop   *watchdog.TurnLoop
	logger *slog.Logger
}

func (g *loopGuard) beforeModel(ctx adkagent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
	stop := g.loop.BeforeModel(ctx.InvocationID())
	if stop == nil {
		return nil, nil
	}
	ts := approval.TurnStopFrom(ctx)
	g.logger.Warn("ending the turn: it reached the per-turn model-call cap",
		"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
		"calls", stop.Calls, "cap", stop.Cap, "can_stop", ts != nil)
	if ts != nil {
		ts.StopTurn(stop)
	}
	// Stands in for the model's answer: text and no function call, so
	// even with no TurnStop to cancel it the flow has nothing left to do
	// and the turn ends here.
	return &model.LLMResponse{
		Content:      genai.NewContentFromText(stop.Error(), genai.RoleModel),
		TurnComplete: true,
	}, nil
}

func (g *loopGuard) beforeTool(ctx adkagent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	stop := g.loop.Before(ctx.InvocationID(), ctx.FunctionCallID(), t.Name(), args)
	if stop == nil {
		return nil, nil
	}
	ts := approval.TurnStopFrom(ctx)
	msg := "refusing a repeated call: the model kept making it after being told it was looping"
	if ts != nil {
		msg = "ending the turn: the model kept repeating a call after being told it was looping"
	}
	g.logger.Warn(msg,
		"session", ctx.SessionID(), "invocation", ctx.InvocationID(),
		"tool", stop.Tool, "count", stop.Count)
	if ts != nil {
		ts.StopTurn(stop)
	}
	return map[string]any{
		loopStoppedKey: loopStopped,
		"detail":       "This call was not made. It repeats the same call with the same arguments and the same result as the calls before it, after you were told the result would not change. The turn is ending.",
	}, nil
}

func (g *loopGuard) afterTool(ctx adkagent.Context, _ tool.Tool, _, result map[string]any, err error) (map[string]any, error) {
	if err != nil {
		// A failed call is the failure-streak signal's business, and
		// carrying a note on it would mean inventing a result map.
		return nil, nil
	}
	if result[loopStoppedKey] == loopStopped {
		// The guard's own refusal. Recorded as a result it would read
		// as a changed one and restart the run, and with no TurnStop to
		// end the turn the model would get a fresh allowance to loop.
		return nil, nil
	}
	note := g.loop.After(ctx.InvocationID(), ctx.FunctionCallID(), result)
	if note == "" {
		return nil, nil
	}
	// A copy, never an edit in place: the map may be one the tool
	// keeps.
	out := make(map[string]any, len(result)+1)
	maps.Copy(out, result)
	out[watchdog.LoopNoteKey] = note
	return out, nil
}
