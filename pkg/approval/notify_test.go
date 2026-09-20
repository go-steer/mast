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
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/mast/pkg/permissions"
)

// announcement is one call of Config.NotifyPark, with the context the
// gate handed it kept alongside the notice. The context is half the
// contract: a send parented on the turn would be cancelled by the very
// thing it reports.
type announcement struct {
	ctx    context.Context
	notice ParkNotice
}

// notifyProbe drives one turn through the real gate and records every
// announcement it produced.
type notifyProbe struct {
	mu sync.Mutex
	// got is every announcement, in order.
	got []announcement
	// entered is closed the first time the hook runs, so a test can wait
	// for the goroutine the gate spawned without a sleep.
	entered chan struct{}
	// release, when non-nil, blocks the hook until it is closed —
	// standing in for an ingress that is slow or hung. Closed on a timer
	// armed before the run rather than by the test, so a gate that
	// regressed to a synchronous send fails on the clock instead of
	// deadlocking the package.
	release chan struct{}
	// runTook is how long the turn took end to end. With release armed,
	// an async send makes this a fraction of releaseAfter and a
	// synchronous one makes it at least releaseAfter.
	runTook time.Duration
	once    sync.Once
}

// releaseAfter is how long a held announcement is held for. Far enough
// above a turn (~0.15s here) that the two cannot be confused, and short
// enough not to pad the suite.
const releaseAfter = 2 * time.Second

func (p *notifyProbe) hook(ctx context.Context, n ParkNotice) {
	p.mu.Lock()
	p.got = append(p.got, announcement{ctx: ctx, notice: n})
	p.mu.Unlock()
	p.once.Do(func() { close(p.entered) })
	if p.release != nil {
		<-p.release
	}
}

func (p *notifyProbe) announcements() []announcement {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]announcement(nil), p.got...)
}

// waitForAnnouncement blocks until the hook has run once. The gate
// announces on its own goroutine, so a test that read the slice straight
// after the run would be racing it.
func (p *notifyProbe) waitForAnnouncement(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the gate parked and never announced it")
	}
}

type notifyProbeConfig struct {
	// policy is the gate's hitl_policy.on_mutation. Empty means
	// require_approval, the only one that parks.
	policy OnMutation
	// release, when true, gives the hook a gate the test must open. Use
	// to hold an announcement in flight while asserting on the turn.
	release bool
	// noHook drops Config.NotifyPark, standing in for a daemon with no
	// egress configured.
	noHook bool
}

// runNotifyProbe runs one turn in which the model proposes a single
// mutating call, and returns the probe plus the confirmation IDs the
// gate wrote into the log.
func runNotifyProbe(t *testing.T, cfg notifyProbeConfig) (*notifyProbe, []string, []scaleArgs) {
	t.Helper()
	probe := &notifyProbe{entered: make(chan struct{})}
	if cfg.release {
		probe.release = make(chan struct{})
	}

	var executed []scaleArgs
	scale, err := functiontool.New(functiontool.Config{
		Name:        "scale_deployment",
		Description: "changes a deployment's replica count",
	}, func(_ adkagent.Context, args scaleArgs) (map[string]any, error) {
		executed = append(executed, args)
		return map[string]any{"scaled": args.Replicas}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	root, err := llmagent.New(llmagent.Config{
		Name:        "notify_agent",
		Description: "park-announcement probe",
		Instruction: "act",
		Model:       &proposingModel{calls: []map[string]any{proposeScale("api", 10)}},
		Tools:       []tool.Tool{scale},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	policy := cfg.policy
	if policy == "" {
		policy = OnMutationRequireApproval
	}
	gcfg := Config{
		Policy:   policy,
		Mutating: alwaysMutating,
		Gate:     permissions.New(permissions.Options{}),
		Workload: "triage",
	}
	if !cfg.noHook {
		gcfg.NotifyPark = probe.hook
	}
	wg, err := New(gcfg)
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}

	svc := sqliteService(t)
	r, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{wg}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	if cfg.release {
		time.AfterFunc(releaseAfter, func() { close(probe.release) })
	}

	// Cancelled the moment the turn is over, which is what a daemon does
	// to a turn's context and what the announcement must survive.
	runCtx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	for _, err := range r.Run(runCtx, testUser, sid, genai.NewContentFromText("scale api to 10", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	probe.runTook = time.Since(started)
	cancel()

	ids, _, _ := confirmationRequests(t, svc)
	return probe, ids, executed
}

// TestAParkAnnouncesItself is #451's core assertion. Every way of
// discovering a park is a pull, so a park that does not push is a
// decision an unattended daemon is waiting on and nobody knows about.
//
// It also pins WHAT is announced: a notice an operator cannot act on is
// a page without an answer, so the session, the tool, the call key and
// the hint all have to be in it.
func TestAParkAnnouncesItself(t *testing.T) {
	probe, ids, executed := runNotifyProbe(t, notifyProbeConfig{})
	probe.waitForAnnouncement(t)

	if len(ids) != 1 {
		t.Fatalf("parks = %d, want 1", len(ids))
	}
	if len(executed) != 0 {
		t.Fatalf("the tool ran %d time(s) while parked; it must not have run at all", len(executed))
	}
	got := probe.announcements()
	if len(got) != 1 {
		t.Fatalf("announcements = %d, want exactly 1 — a park announced twice pages an operator twice for one decision", len(got))
	}
	n := got[0].notice
	if n.Session != sid {
		t.Errorf("Session = %q, want %q — without it there is nothing to answer", n.Session, sid)
	}
	if n.Invocation == "" {
		t.Error("Invocation is empty")
	}
	if n.Workload != "triage" {
		t.Errorf("Workload = %q, want %q", n.Workload, "triage")
	}
	if n.Agent != "notify_agent" {
		t.Errorf("Agent = %q, want %q", n.Agent, "notify_agent")
	}
	if n.Tool != "scale_deployment" {
		t.Errorf("Tool = %q, want %q", n.Tool, "scale_deployment")
	}
	if want := CallKey("scale_deployment", proposeScale("api", 10)); n.Key != want {
		t.Errorf("Key = %q, want %q — the notice has to name the same call GET /parks does", n.Key, want)
	}
	if !strings.Contains(n.Hint, "Approve mutating call") {
		t.Errorf("Hint = %q, want the operator-facing park hint", n.Hint)
	}
	if n.ParkedAt.IsZero() {
		t.Error("ParkedAt is zero")
	}
}

// TestTheAnnouncementOutlivesTheTurnThatRaisedIt is the reason the gate
// detaches the context instead of passing the turn's along.
//
// A park is the end of a turn's useful work, so the context that raised
// it is at its closest to cancelled at the exact moment the
// announcement is worth sending. Parenting the send on it would mean the
// announcement most likely to matter — the one from a turn being cut by
// a budget, a drain or the watchdog — is the one that never goes out.
func TestTheAnnouncementOutlivesTheTurnThatRaisedIt(t *testing.T) {
	// Held open deliberately. The gate bounds the announcement itself and
	// cancels when the hook returns, so an assertion made after the hook
	// had finished would read that cleanup rather than the turn — it
	// would pass on code that had inherited the turn's cancellation, and
	// fail on code that had not.
	probe, _, _ := runNotifyProbe(t, notifyProbeConfig{release: true})
	probe.waitForAnnouncement(t)

	got := probe.announcements()
	if len(got) != 1 {
		t.Fatalf("announcements = %d, want 1", len(got))
	}
	// runNotifyProbe cancelled the run's context before returning, and
	// the send is still in flight.
	if err := got[0].ctx.Err(); err != nil {
		t.Fatalf("the announcement's context is already done (%v): it inherited the turn's cancellation, so a park raised by a turn that is being cut would never be announced", err)
	}
	if _, ok := got[0].ctx.Deadline(); !ok {
		t.Error("the announcement's context has no deadline: a hung ingress would leak this goroutine for the life of the process")
	}
}

// TestTheGateDoesNotWaitForTheAnnouncement pins the other half of the
// contract. A park has to stay answerable while its announcement is
// failing, so a slow or hung ingress must not hold the turn open — which
// it would, if the send were synchronous.
func TestTheGateDoesNotWaitForTheAnnouncement(t *testing.T) {
	probe, ids, _ := runNotifyProbe(t, notifyProbeConfig{release: true})
	probe.waitForAnnouncement(t)
	if probe.runTook >= releaseAfter {
		t.Errorf("the turn took %s, which is the whole time the ingress was hung: the gate is waiting for the announcement it is sending, so a slow or dead chat ingress now stalls every park", probe.runTook)
	}
	if len(ids) != 1 {
		t.Fatalf("parks = %d, want 1 — the park must be recorded and answerable with the announcement still in flight", len(ids))
	}
}

// TestACallThatDoesNotParkAnnouncesNothing keeps the announcement tied
// to the thing it reports. Under `apply` the call runs, nobody is
// waiting on anybody, and a message saying mast stopped to ask would be
// false.
func TestACallThatDoesNotParkAnnouncesNothing(t *testing.T) {
	probe, ids, executed := runNotifyProbe(t, notifyProbeConfig{policy: OnMutationApply})
	if len(ids) != 0 {
		t.Fatalf("parks = %d, want 0 under apply", len(ids))
	}
	if len(executed) != 1 {
		t.Fatalf("executions = %d, want 1 under apply", len(executed))
	}
	if got := probe.announcements(); len(got) != 0 {
		t.Fatalf("announcements = %d, want 0: nothing is waiting for an operator", len(got))
	}
}

// TestNoNotifierIsNotAnError covers every composition that configured no
// egress — a library embed, a one-shot, a daemon whose operator did not
// ask. The park behaves exactly as it did before #451.
func TestNoNotifierIsNotAnError(t *testing.T) {
	probe, ids, executed := runNotifyProbe(t, notifyProbeConfig{noHook: true})
	if len(ids) != 1 {
		t.Fatalf("parks = %d, want 1", len(ids))
	}
	if len(executed) != 0 {
		t.Fatalf("the tool ran %d time(s) while parked", len(executed))
	}
	if got := probe.announcements(); len(got) != 0 {
		t.Fatalf("announcements = %d, want 0", len(got))
	}
}
