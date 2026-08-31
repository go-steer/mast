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

package transcript

import (
	"context"
	"testing"

	adksession "google.golang.org/adk/v2/session"
)

// dispatchApp is the AppName pkg/planner gives the runner it builds
// inside invoke_specialist. Duplicated as a literal rather than imported
// so this package stays dependency-light and so a rename upstream shows
// up here as a failing probe rather than as silent agreement.
const dispatchApp = "planner_dispatch"

// W9.3 probe (#235). "Just make the dispatch sub-session durable" is the
// sentence most likely to be said about the missing outbox record under
// hitl.on_mutation: apply. This measures what durability alone would buy,
// and the answer is nothing: the boot-time auto-resume scan that consumes
// dangling intents (cmd/mast/autoresume.go → Store.ScanInterrupted) lists
// sessions for ONE AppName, and a dispatch sub-runner does not use the
// workload's.
//
// So a durable sub-session is invisible to the pass that exists to act on
// it. Whatever closes W9.3 has to put the record somewhere the workload's
// own scan already looks — which is the outer session — not merely
// somewhere that survives a restart.
func TestScanInterruptedDoesNotSeeADispatchSubSession(t *testing.T) {
	for name, svc := range services(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			workload := NewStore(svc, testApp)
			dispatch := NewStore(svc, dispatchApp)

			// One interrupted session per app, seeded identically. The
			// only difference is the AppName.
			seed(t, svc, "op", "s-outer", textEvent("planner", "dispatching..."))
			if _, err := svc.Create(ctx, &adksession.CreateRequest{
				AppName:   dispatchApp,
				UserID:    "op",
				SessionID: "invoke-exec",
			}); err != nil {
				t.Fatalf("create dispatch sub-session: %v", err)
			}

			if err := workload.MarkInterrupted(ctx, "op", "s-outer", "daemon shutdown (SIGTERM)"); err != nil {
				t.Fatalf("MarkInterrupted(s-outer): %v", err)
			}
			if err := dispatch.MarkInterrupted(ctx, "op", "invoke-exec", "daemon shutdown (SIGTERM)"); err != nil {
				t.Fatalf("MarkInterrupted(invoke-exec): %v", err)
			}

			got, err := workload.ScanInterrupted(ctx)
			if err != nil {
				t.Fatalf("ScanInterrupted: %v", err)
			}
			for _, c := range got {
				if c.SessionID == "invoke-exec" {
					t.Fatalf("the workload's boot scan now sees a dispatch sub-session; W9.3's premise " +
						"changed and the design must be re-derived")
				}
			}
			if len(got) != 1 || got[0].SessionID != "s-outer" {
				t.Fatalf("workload scan = %+v, want exactly the outer session", got)
			}

			// The sub-session really is there and really is marked — the
			// scan's blindness is app scoping, not a seeding mistake. A
			// probe that passed because nothing was written would be
			// measuring nothing.
			viaDispatch, err := dispatch.ScanInterrupted(ctx)
			if err != nil {
				t.Fatalf("ScanInterrupted(dispatch app): %v", err)
			}
			if len(viaDispatch) != 1 || viaDispatch[0].SessionID != "invoke-exec" {
				t.Fatalf("dispatch-app scan = %+v, want exactly invoke-exec; the probe did not seed what it thinks", viaDispatch)
			}
		})
	}
}
