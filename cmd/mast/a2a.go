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

package main

// A2A-server wiring for the daemon (docs/a2a-design.md, "Mast as A2A
// server"). pkg/a2a owns the wire protocol, auth, and HTTP surface but
// never imports the runtime; this file supplies the Backend seam —
// GetTask over the transcript store's state projection and CancelTask
// over the same abort machinery the /abort door uses — and projects the
// bundle's a2a: section into the exposed skills.
//
// Stage A serves the card, tasks/get, and tasks/cancel behind pluggable
// bearer auth. message/send (turn execution through runTurnPre) is Stage
// B; message/stream is Stage C.

import (
	"context"
	"errors"
	"net"
	"os"

	"log/slog"

	buildversion "github.com/go-steer/mast/internal/version"
	"github.com/go-steer/mast/pkg/a2a"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// a2aBackend implements a2a.Backend over the daemon's transcript store
// and abort machinery. A task id IS a mast session id; there is one
// exposed workload per single-session daemon (Stage A), so every task
// resolves to workloadName for the scope check.
type a2aBackend struct {
	store        *transcript.Store
	obs          *observability.Registry
	tracker      *turnTracker
	logger       *slog.Logger
	workloadName string
}

// GetTask snapshots a task's state from the session's log-proven state.
// A transcript-only read never reports "completed" — the event log
// cannot prove a turn finished versus is mid-flight — so idle sessions
// map to "working" (the in-process task registry supplies "completed" in
// Stage B).
func (b *a2aBackend) GetTask(ctx context.Context, taskID string) (a2a.TaskInfo, error) {
	if transcript.IsReservedSessionID(taskID) {
		return a2a.TaskInfo{}, a2a.ErrTaskNotFound
	}
	d, err := b.store.Get(ctx, "", taskID)
	if err != nil {
		if errors.Is(err, transcript.ErrNotFound) {
			return a2a.TaskInfo{}, a2a.ErrTaskNotFound
		}
		return a2a.TaskInfo{}, err
	}
	state, msg := mapTranscriptState(d)
	return a2a.TaskInfo{WorkloadName: b.workloadName, State: state, StatusMessage: msg}, nil
}

// CancelTask routes to the same terminal-abort path the /abort door
// uses: marker first (the durable truth), then sweep the in-flight
// turn's cancel handle. Idempotent — a second cancel of an
// already-aborted session succeeds and re-reports canceled.
func (b *a2aBackend) CancelTask(ctx context.Context, taskID, reason string) (a2a.TaskInfo, error) {
	if transcript.IsReservedSessionID(taskID) {
		return a2a.TaskInfo{}, a2a.ErrTaskNotFound
	}
	err := recordAbort(ctx, b.store, b.obs, b.workloadName, taskID, reason)
	switch {
	case err == nil:
		// New abort landed; sweep any in-flight turn.
		if b.tracker.cancelSession(taskID) {
			b.logger.Info("a2a tasks/cancel cancelled in-flight turn", "task", taskID)
		}
	case errors.Is(err, transcript.ErrAlreadyAborted):
		// Idempotent: already terminal, nothing to sweep.
	case errors.Is(err, transcript.ErrNotFound):
		return a2a.TaskInfo{}, a2a.ErrTaskNotFound
	default:
		return a2a.TaskInfo{}, err
	}
	// Re-read for the resulting snapshot. If the read fails after a
	// successful marker write, report canceled synthetically — the abort
	// is the durable truth regardless of a transient read error.
	d, gerr := b.store.Get(ctx, "", taskID)
	if gerr != nil {
		return a2a.TaskInfo{WorkloadName: b.workloadName, State: a2a.TaskStateCanceled, StatusMessage: reason}, nil
	}
	state, msg := mapTranscriptState(d)
	return a2a.TaskInfo{WorkloadName: b.workloadName, State: state, StatusMessage: msg}, nil
}

// mapTranscriptState projects mast's log-proven session state onto the
// A2A task lifecycle (docs/a2a-design.md "State mapping"). It never
// returns "completed": the transcript cannot prove a turn finished.
func mapTranscriptState(d *transcript.Detail) (a2a.TaskState, string) {
	switch d.State {
	case transcript.StateAborted:
		return a2a.TaskStateCanceled, d.AbortReason
	case transcript.StatePaused:
		// A pending (unresolved) interrupt is a HITL request the task is
		// blocked on — input-required. A gate-only pause (timed/operator,
		// no pending interrupt) is still the daemon's own commitment —
		// working.
		if len(d.PendingInterruptIDs) > 0 {
			return a2a.TaskStateInputRequired, d.PauseMessage
		}
		return a2a.TaskStateWorking, d.PauseMessage
	case transcript.StateInterrupted:
		return a2a.TaskStateWorking, d.InterruptReason
	default: // StateIdle — cannot prove completed from the log alone.
		return a2a.TaskStateWorking, ""
	}
}

// a2aExposedSkills projects the bundle's a2a: section into the server's
// exposed-skill list. Empty (nil) when the workload does not opt in.
func a2aExposedSkills(bundle *workload.Bundle) []a2a.ExposedSkill {
	if bundle == nil || !bundle.A2A.Expose {
		return nil
	}
	skillName := bundle.A2A.SkillName
	if skillName == "" {
		skillName = bundle.Name
	}
	desc := bundle.A2A.SkillDescription
	if desc == "" {
		desc = bundle.Description
	}
	return []a2a.ExposedSkill{{
		WorkloadName: bundle.Name,
		SkillName:    skillName,
		Description:  desc,
		Scopes:       bundle.A2A.Auth.Scopes,
	}}
}

// a2aValidator builds the endpoint's token validator from MAST_A2A_TOKEN.
// Unset means unauthenticated (dev only), mirroring the inject door. The
// static principal carries the union of every exposed skill's scopes so
// it can invoke any of them.
func a2aValidator(logger *slog.Logger, skills []a2a.ExposedSkill) (a2a.TokenValidator, error) {
	token := os.Getenv("MAST_A2A_TOKEN")
	if token == "" {
		logger.Warn("MAST_A2A_TOKEN not set; A2A endpoint is unauthenticated (dev only)")
		return nil, nil
	}
	seen := map[string]bool{}
	var scopes []string
	for _, sk := range skills {
		for _, s := range sk.Scopes {
			if !seen[s] {
				seen[s] = true
				scopes = append(scopes, s)
			}
		}
	}
	return a2a.NewStaticBearerValidator(map[string]*a2a.Principal{
		token: {Subject: "mast-a2a-static", Scopes: scopes},
	})
}

// buildA2AServer constructs the A2A server for the daemon, or (nil, nil)
// when no workload opts into A2A exposure (the server is simply not
// started). baseCtx is the daemon's turn lifetime.
func buildA2AServer(
	logger *slog.Logger,
	listen string,
	bundle *workload.Bundle,
	backend a2a.Backend,
	metric a2a.TaskMetric,
	baseCtx context.Context,
) (*a2a.Server, error) {
	skills := a2aExposedSkills(bundle)
	if len(skills) == 0 {
		logger.Info("A2A listener requested but no workload opts into A2A exposure (a2a.expose); A2A disabled")
		return nil, nil
	}
	validator, err := a2aValidator(logger, skills)
	if err != nil {
		return nil, err
	}
	desc := ""
	if bundle != nil {
		desc = bundle.Description
	}
	return a2a.New(a2a.Config{
		Listen:          listen,
		Skills:          skills,
		Validator:       validator,
		Backend:         backend,
		CardName:        "mast",
		CardDescription: desc,
		CardVersion:     buildversion.Version,
		Metric:          metric,
		Logger:          logger,
		BaseContext:     baseCtx,
	})
}

// a2aListener binds the A2A server's listener eagerly so a bad bind
// address fails serve() at startup rather than in a background goroutine
// (mirrors buildAttach).
func a2aListener(listen string) (net.Listener, error) {
	return net.Listen("tcp", listen)
}
