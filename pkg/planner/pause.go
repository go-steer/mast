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

package planner

import (
	"context"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/mast/pkg/transcript"
)

// PauseRecorder mints the durable pause record + resume token for a
// plane-A interrupt pause. *transcript.Store implements it.
type PauseRecorder interface {
	PauseInterrupt(ctx context.Context, userID, sessionID, interruptID string, spec transcript.PauseSpec) (transcript.PauseHandle, error)
}

// pauseSessionArgs is the pinned schema of pause_session — the
// PauseSpec subset the model may set. The token TTL is deliberately
// absent: an agent cannot size its own token's lifetime; operators
// extend through the audited surface.
type pauseSessionArgs struct {
	// Reason is the pause-reason enum value; see
	// transcript.ValidReasons. Typically "ambiguity" or
	// "cost_cool_down" for a planner-initiated pause.
	Reason string `json:"reason"`
	// Message states, for the operator, why the session paused and
	// what would justify resuming it.
	Message string `json:"message"`
	// ResumeAt (RFC3339) optionally arms the timed-pause scheduler.
	ResumeAt string `json:"resume_at,omitempty"`
}

// newPauseSessionTool builds the plane-A self-pause: a long-running
// tool whose nil return parks the turn exactly like
// request_operator_input (the pending call ID lands in
// Event.LongRunningToolIDs; the wrapper node parks). The body writes
// the pause record — keyed by its own function-call ID — to the
// companion ops row BEFORE returning nil, so the park always has its
// token. On a record-write failure the tool returns an error result,
// NEVER nil (adversarial-gate finding M4): the FunctionResponse closes
// the would-be interrupt, so there is no park without a token, and the
// model sees the failure.
//
// Note the plane-A contract (docs/durable-execution-design.md):
// pause_session parks this TURN, not the session — unrelated inject or
// attach turns still run (and a dangling tool_use is provider-invalid
// on Anthropic-family models). Session-stopping semantics are the gate
// pause's job.
func newPauseSessionTool(rec PauseRecorder) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: ToolPauseSession,
		Description: "Pause this run durably and hand control to an operator. Use when work " +
			"should not proceed without review (reason: ambiguity) or spend approval " +
			"(reason: cost_cool_down). State in message exactly why you paused and what " +
			"would justify resuming; optionally set resume_at (RFC3339) to resume on a timer. " +
			"An operator resumes with the pause's token or interrupt ID.",
		IsLongRunning: true,
	}, func(ctx adkagent.Context, args pauseSessionArgs) (map[string]any, error) {
		spec := transcript.PauseSpec{
			Reason:  transcript.Reason(args.Reason),
			Message: args.Message,
		}
		if args.ResumeAt != "" {
			at, err := time.Parse(time.RFC3339, args.ResumeAt)
			if err != nil {
				return map[string]any{
					"status": "error",
					"error":  "resume_at must be RFC3339 (e.g. 2026-08-02T22:00:00Z): " + err.Error(),
				}, nil
			}
			spec.ResumeAt = at.UTC()
		}
		if _, err := rec.PauseInterrupt(ctx, ctx.UserID(), ctx.SessionID(), ctx.FunctionCallID(), spec); err != nil {
			return map[string]any{
				"status": "error",
				"error":  "pause record write failed; the run continues un-paused: " + err.Error(),
			}, nil
		}
		// nil result from a long-running tool IS the park.
		return nil, nil
	})
}
