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

package cli

import (
	"fmt"
	"log/slog"
	"os"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/internal/modeltext"
)

func logEvent(logger *slog.Logger, event *session.Event, sessionID string) {
	if event == nil {
		return
	}
	if event.RequestedInput != nil {
		logger.Info("HITL PAUSE",
			"session", sessionID,
			"interrupt_id", event.RequestedInput.InterruptID,
			"message", event.RequestedInput.Message,
		)
	} else if len(event.LongRunningToolIDs) > 0 {
		// Tool-level pause (e.g. the planner's request_operator_input):
		// the pending function-call ID is the interrupt ID an operator
		// passes to POST /resume.
		logger.Info("HITL PAUSE (long-running tool)",
			"session", sessionID,
			"interrupt_ids", event.LongRunningToolIDs,
		)
	}
	summary := "(no text)"
	if event.Content != nil {
		for _, part := range event.Content.Parts {
			if part == nil {
				continue
			}
			if text, ok := modeltext.Text(part); ok {
				summary = text
				break
			}
			if part.FunctionCall != nil {
				summary = "function_call:" + part.FunctionCall.Name
				break
			}
			if part.FunctionResponse != nil {
				summary = "function_response:" + part.FunctionResponse.Name
				break
			}
		}
	}
	attrs := []any{
		"session", sessionID,
		"author", event.Author,
		"branch", event.Branch,
		"summary", summary,
	}
	if event.Output != nil {
		attrs = append(attrs, "output", fmt.Sprintf("%v", event.Output))
	}
	logger.Info("runner event", attrs...)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
