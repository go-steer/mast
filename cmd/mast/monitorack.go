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

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	adkagent "google.golang.org/adk/v2/agent"

	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/monitor"
	"github.com/go-steer/mast/pkg/observability"
	"github.com/go-steer/mast/pkg/transcript"
	"github.com/go-steer/mast/pkg/workload"
)

// The ack leg (v0.5 W4.6): an operator says "I have seen this one",
// and the monitoring stops reporting it for a while.
//
// # An ack is not an approval, and this file is where that is true
//
// They share an operator, a chat window and a verb, and they share
// nothing else. An approval mints a grant that licenses a write to the
// world and is consumed on use; an ack asserts no diagnosis, authorizes
// no change, and does one thing — asks whoever owns the finding state
// to stop surfacing one subject. Nothing here touches pkg/permissions
// or pkg/approval, and nothing here writes a decision record. If it
// did, the answer to "who approved this change" — the question v0.3
// spent a release making precise — would start including people who
// muted an alert.
//
// # Three legs, in this order, and the order is the design
//
//  1. Attribute. Who is asking comes from the credential the request
//     presented, resolved by pkg/inject's callerContext. Never from the
//     body: an attribution a caller writes about itself is worth
//     nothing after an incident, which is the rule #194 settled for
//     approvals and this inherits.
//  2. Record. mast writes its durable ack record BEFORE forwarding.
//     mast is the store of record for who asked and when; the producer
//     is the store of record for the suppression. Recording first means
//     a forward that fails leaves an attributed ack marked
//     Forwarded=false — visibly a suppression that did not take, rather
//     than an operator action with no trace.
//  3. Forward. The bundle's monitor.ack tool, run on mast's own behalf
//     through the same direct-run seam the collection leg uses, with
//     subject_key and ack_by filled in by mast over the bundle's
//     literal args.
//
// # Why mast is in the path at all
//
// It would be simpler for the chat transport to call the producer
// directly. Two things break if it does. The transport grows a second
// backend with a second auth model, for one route. And the ack becomes
// unattributed by construction — the producer's ack surface takes an
// ack_by string from whoever calls it and cannot check it, so the only
// place an ack can be tied to an authenticated human is the service
// that authenticated them. See docs/orchestration-design.md, "Ack
// routing".

// monitorAcker forwards operator acknowledgements to the tool the
// bundle names, and records who asked.
type monitorAcker struct {
	tool string
	args map[string]any

	run    func(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error)
	store  *transcript.Store
	obs    *observability.Registry
	logger *slog.Logger

	appName      string
	userID       string
	workloadName string

	// now is the injection point for the recorded timestamp, the same
	// seam the collector and the scheduled trigger use.
	now func() time.Time
}

func newMonitorAcker(logger *slog.Logger, obs *observability.Registry, store *transcript.Store, mon workload.Monitor, run func(adkagent.Context, string, map[string]any) (map[string]any, error), workloadName, appName, userID string) *monitorAcker {
	return &monitorAcker{
		tool:         mon.AckTool(),
		args:         mon.AckArgs(),
		run:          run,
		store:        store,
		obs:          obs,
		logger:       logger,
		appName:      appName,
		userID:       userID,
		workloadName: workloadName,
		now:          func() time.Time { return time.Now().UTC() },
	}
}

// enabled reports whether this workload takes acks at all. A nil acker
// is a workload with no monitor.ack block, so callers do not have to
// check for both — and the route it backs answers 404 rather than
// pretending to have suppressed something.
func (a *monitorAcker) enabled() bool {
	return a != nil && a.tool != "" && a.run != nil
}

func (a *monitorAcker) clock() time.Time {
	if a == nil || a.now == nil {
		return time.Now().UTC()
	}
	return a.now()
}

// forward applies one acknowledgement: attribute, record, forward.
func (a *monitorAcker) forward(ctx context.Context, req inject.MonitorAckRequest) (inject.MonitorAckResult, error) {
	if !a.enabled() {
		return inject.MonitorAckResult{}, errors.New("this workload declares no monitor.ack tool")
	}
	// A relay with several mast deployments configured should fail
	// loudly on the wrong one rather than suppress a finding on a
	// cluster nobody was looking at.
	if req.Workload != "" && req.Workload != a.workloadName {
		return inject.MonitorAckResult{}, fmt.Errorf("this daemon serves workload %q, not %q: %w", a.workloadName, req.Workload, inject.ErrBadPayload)
	}

	caller, ok := auth.CallerFromContext(ctx)
	if !ok || caller.Identity == "" {
		// Unreachable from the route — pkg/inject attributes at least
		// the shared credential — and refused rather than defaulted
		// anyway. An ack whose whole purpose is to be attributable must
		// not fall back to a mechanism name the way an internal resume
		// legitimately does.
		return inject.MonitorAckResult{}, fmt.Errorf("no authenticated caller on this request, so the ack cannot be attributed to anyone: %w", inject.ErrBadPayload)
	}
	proxyBy, _ := auth.ProxyByFromContext(ctx)

	// Read the prior ack before writing over it, so the answer can say
	// who already asked for quiet. Informational only: a repeat ack is
	// forwarded regardless, because whether a second one is redundant
	// is the producer's call and not mast's.
	var prevBy, prevAt string
	if prev := a.store.Ack(ctx, a.userID, a.workloadName, req.Subject); prev != nil {
		prevBy, prevAt = prev.By, prev.At.Format(time.RFC3339)
	}

	rec := transcript.AckRecord{
		Workload: a.workloadName,
		Subject:  req.Subject,
		By:       caller.Identity,
		ProxyBy:  proxyBy,
		Reason:   req.Reason,
		At:       a.clock(),
	}
	if err := a.store.RecordAck(ctx, a.userID, rec); err != nil {
		// Refused, not logged-and-continued. The durable record is
		// mast's entire half of this exchange; forwarding without it
		// would suppress a finding with no trace of who asked, which is
		// the one outcome the whole path exists to prevent.
		a.obs.MonitorAck(a.workloadName, observability.MonitorAckError)
		return inject.MonitorAckResult{}, fmt.Errorf("recording the ack durably: %w", err)
	}

	// The ack context is the collection leg's: no session, no
	// invocation, no model. The sessionID is empty because there is no
	// cycle in flight — an ack arrives when an operator reads their
	// chat, which is rarely the moment a fire is running.
	actx := newCollectContext(ctx, a.appName, a.userID, "")
	args := make(map[string]any, len(a.args)+2)
	for k, v := range a.args {
		args[k] = v
	}
	// Last, and deliberately after the bundle's literals: the loader
	// already refuses a bundle that names either of these, and this is
	// the belt to that braces. An operator identity a YAML file could
	// override is not an identity.
	args[monitor.AckSubjectArg] = req.Subject
	args[monitor.AckByArg] = caller.Identity
	if req.Reason != "" {
		if _, taken := args["reason"]; !taken {
			args["reason"] = req.Reason
		}
	}

	if _, err := a.run(actx, a.tool, args); err != nil {
		a.obs.MonitorAck(a.workloadName, observability.MonitorAckError)
		// The record stays, with Forwarded false. An operator asking
		// again is the recovery, and the audit shows both attempts.
		a.logger.Error("operator ack recorded but not forwarded; the suppression did not take",
			"workload", a.workloadName, "subject", req.Subject, "ack_by", caller.Identity,
			"tool", a.tool, "error", err.Error())
		return inject.MonitorAckResult{}, fmt.Errorf("forwarding the ack to %s: %w", a.tool, err)
	}

	rec.Forwarded = true
	if err := a.store.RecordAck(ctx, a.userID, rec); err != nil {
		// The suppression HAS taken; only mast's note that it did is
		// missing. Logged rather than returned: answering an error here
		// would have an operator ack a second time to fix a bookkeeping
		// row, suppressing something twice to correct a record.
		a.logger.Warn("ack forwarded but its durable record still reads unforwarded",
			"workload", a.workloadName, "subject", req.Subject, "error", err.Error())
	}
	a.obs.MonitorAck(a.workloadName, observability.MonitorAckForwarded)
	a.logger.Info("operator ack forwarded",
		"workload", a.workloadName, "subject", req.Subject,
		"ack_by", auth.Attribution(ctx, caller.Identity), "tool", a.tool)

	return inject.MonitorAckResult{
		Workload:          a.workloadName,
		Subject:           req.Subject,
		AckBy:             caller.Identity,
		PreviouslyAckedBy: prevBy,
		PreviouslyAckedAt: prevAt,
	}, nil
}
