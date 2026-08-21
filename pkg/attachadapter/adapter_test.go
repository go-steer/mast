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

package attachadapter

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/mast/pkg/attach"
	"github.com/go-steer/mast/pkg/auth"
	"github.com/go-steer/mast/pkg/digest"
	"github.com/go-steer/mast/pkg/eventlog"
)

// testHandle opens a throwaway SQLite eventlog so Config validation
// passes; adapter tests never read it.
func testHandle(t *testing.T) *eventlog.Handle {
	t.Helper()
	h, err := eventlog.Open(t.Context(), sqlite.Open(filepath.Join(t.TempDir(), "el.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func baseConfig(t *testing.T, run func(ctx context.Context, msg string) (TurnResult, error)) Config {
	t.Helper()
	return Config{
		AppName:   "mast",
		UserID:    "op",
		SessionID: "s1",
		EventLog:  testHandle(t),
		RunTurn:   run,
	}
}

func TestNewValidation(t *testing.T) {
	run := func(context.Context, string) (TurnResult, error) { return TurnResult{}, nil }
	for name, mutate := range map[string]func(*Config){
		"missing session triple": func(c *Config) { c.SessionID = "" },
		"missing eventlog":       func(c *Config) { c.EventLog = nil },
		"missing runturn":        func(c *Config) { c.RunTurn = nil },
	} {
		cfg := baseConfig(t, run)
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted an invalid Config", name)
		}
	}
	if _, err := New(baseConfig(t, run)); err != nil {
		t.Fatalf("New rejected a valid Config: %v", err)
	}
}

// TestInjectSerializesTurns proves two concurrent Injects never
// overlap RunTurn calls and both run in order.
func TestInjectSerializesTurns(t *testing.T) {
	var mu sync.Mutex
	var order []string
	inFlight := 0
	maxInFlight := 0
	done := make(chan struct{}, 2)

	ad, err := New(baseConfig(t, func(_ context.Context, msg string) (TurnResult, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		order = append(order, msg)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		done <- struct{}{}
		return TurnResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	if err := ad.Inject("first"); err != nil {
		t.Fatal(err)
	}
	if err := ad.Inject("second"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("turns did not complete")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxInFlight != 1 {
		t.Errorf("turns overlapped: max in flight = %d, want 1", maxInFlight)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("turn order = %v, want [first second]", order)
	}
}

// TestInjectAsPropagatesCaller proves the injected caller rides the
// turn context.
func TestInjectAsPropagatesCaller(t *testing.T) {
	got := make(chan auth.Caller, 1)
	ad, err := New(baseConfig(t, func(ctx context.Context, _ string) (TurnResult, error) {
		c, _ := auth.CallerFromContext(ctx)
		got <- c
		return TurnResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.InjectAs("hi", auth.Caller{Identity: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-got:
		if c.Identity != "alice@example.com" {
			t.Errorf("caller identity = %q, want alice@example.com", c.Identity)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn never ran")
	}
}

// TestAttachInterruptCancelsTurn proves POST /interrupt semantics:
// the in-flight turn's ctx is canceled; idle interrupt reports false.
func TestAttachInterruptCancelsTurn(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan error, 1)
	ad, err := New(baseConfig(t, func(ctx context.Context, _ string) (TurnResult, error) {
		close(started)
		<-ctx.Done()
		finished <- ctx.Err()
		return TurnResult{}, ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}

	if ad.AttachInterrupt() {
		t.Error("AttachInterrupt with no turn in flight returned true")
	}
	// The audit append targets the session row, which in production
	// the runner creates on the first turn; the stub RunTurn doesn't,
	// so create it here (the audit is best-effort and silently skips
	// sessions that don't exist yet).
	if _, err := ad.cfg.EventLog.Service.Create(context.Background(), &session.CreateRequest{
		AppName: ad.cfg.AppName, UserID: ad.cfg.UserID, SessionID: ad.cfg.SessionID,
	}); err != nil {
		t.Fatalf("create session row: %v", err)
	}
	if err := ad.Inject("long turn"); err != nil {
		t.Fatal(err)
	}
	<-started
	if !ad.AttachInterrupt() {
		t.Error("AttachInterrupt with a turn in flight returned false")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("turn ctx err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt did not cancel the turn")
	}

	// The adapter self-audits interrupts (attach.InterruptSelfAuditor):
	// drain appends the audit event after the interrupted turn
	// returns. Wait for it — both to verify the behavior and because
	// returning earlier races t.TempDir cleanup against the write.
	deadline := time.Now().Add(5 * time.Second)
	for !hasInterruptAudit(t, ad) {
		if time.Now().After(deadline) {
			t.Fatal("no attach/interrupt audit event appeared after the interrupted turn returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// hasInterruptAudit reports whether the adapter's session carries an
// operator-interrupt audit event.
func hasInterruptAudit(t *testing.T, ad *Adapter) bool {
	t.Helper()
	resp, err := ad.cfg.EventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName:   ad.cfg.AppName,
		UserID:    ad.cfg.UserID,
		SessionID: ad.cfg.SessionID,
	})
	if err != nil {
		return false
	}
	for ev := range resp.Session.Events().All() {
		if ev.Author == "attach/interrupt" {
			return true
		}
	}
	return false
}

// TestOperatorEventSequence proves the emitter sees the spec order:
// status streaming → turn-complete → status idle, and turn-error on
// failure.
func TestOperatorEventSequence(t *testing.T) {
	turnErr := errors.New("boom")
	fail := false
	done := make(chan struct{}, 1)
	ad, err := New(baseConfig(t, func(context.Context, string) (TurnResult, error) {
		defer func() { done <- struct{}{} }()
		if fail {
			return TurnResult{}, turnErr
		}
		return TurnResult{TokensIn: 3, TokensOut: 7}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var types []string
	var payloads []any
	ad.SetOperatorEventEmitter(func(eventType string, payload any) {
		mu.Lock()
		types = append(types, eventType)
		payloads = append(payloads, payload)
		mu.Unlock()
	})

	waitIdle := func() {
		t.Helper()
		<-done
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			n := len(types)
			last := ""
			if n > 0 {
				last = types[n-1]
			}
			mu.Unlock()
			if last == attach.EventStatusUpdate && n >= 3 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("emitter never saw the terminal status frame; got %v", types)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	if err := ad.Inject("ok turn"); err != nil {
		t.Fatal(err)
	}
	waitIdle()
	mu.Lock()
	if len(types) != 3 || types[0] != attach.EventStatusUpdate || types[1] != attach.EventTurnComplete || types[2] != attach.EventStatusUpdate {
		t.Fatalf("event sequence = %v, want [status-update turn-complete status-update]", types)
	}
	tc, ok := payloads[1].(attach.TurnComplete)
	if !ok || tc.TokensIn != 3 || tc.TokensOut != 7 || tc.PromptID == "" {
		t.Errorf("turn-complete payload = %+v, want tokens 3/7 and a prompt id", payloads[1])
	}
	types, payloads = nil, nil
	mu.Unlock()

	fail = true
	if err := ad.Inject("bad turn"); err != nil {
		t.Fatal(err)
	}
	waitIdle()
	mu.Lock()
	defer mu.Unlock()
	if len(types) != 3 || types[1] != attach.EventTurnError {
		t.Fatalf("event sequence = %v, want turn-error in the middle", types)
	}
}

// TestStatusReflectsTurnState proves AttachStatus flips running/idle.
func TestStatusReflectsTurnState(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	ad, err := New(baseConfig(t, func(context.Context, string) (TurnResult, error) {
		close(started)
		<-release
		return TurnResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := ad.AttachStatus().State; got != "idle" {
		t.Errorf("initial state = %q, want idle", got)
	}
	if err := ad.Inject("x"); err != nil {
		t.Fatal(err)
	}
	<-started
	if got := ad.AttachStatus().State; got != "running" {
		t.Errorf("in-turn state = %q, want running", got)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for ad.AttachStatus().State != "idle" {
		if time.Now().After(deadline) {
			t.Fatal("state never returned to idle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The capability report is a statement about what is WIRED, not about
// what the Go type structurally satisfies (core-agent #490): the
// adapter implements every optional interface unconditionally, so
// interface probing would advertise a guardrail surface on a daemon
// that passed no guardrail funcs, and the operator's reset would come
// back 200-with-nothing-reset.
func TestAttachCapabilitiesReportsOnlyWiredFuncs(t *testing.T) {
	run := func(context.Context, string) (TurnResult, error) { return TurnResult{}, nil }

	bare, err := New(baseConfig(t, run))
	if err != nil {
		t.Fatal(err)
	}
	got := bare.AttachCapabilities()
	if !got.Interrupt {
		t.Error("Interrupt = false; the adapter always services it")
	}
	if got.Guardrails || got.CostCeiling {
		t.Errorf("unwired adapter reports %+v", got)
	}

	// A read without a reset is not a guardrail surface: the flag gates
	// whether a client offers the button.
	cfg := baseConfig(t, run)
	cfg.GuardrailsFn = func() attach.GuardrailInfo { return attach.GuardrailInfo{} }
	readOnly, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := readOnly.AttachCapabilities(); got.Guardrails {
		t.Errorf("guardrails advertised with no reset func: %+v", got)
	}

	// cost_ceiling is a claim about this session's configuration, not
	// about the endpoint — a workload with no `budget:` block has the
	// surface wired and no ceiling to trip.
	cfg = baseConfig(t, run)
	cfg.GuardrailsFn = func() attach.GuardrailInfo { return attach.GuardrailInfo{} }
	cfg.ResetGuardrailFn = func(attach.GuardrailResetRequest) (attach.GuardrailResetResponse, error) {
		return attach.GuardrailResetResponse{}, nil
	}
	unbounded, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := unbounded.AttachCapabilities(); !got.Guardrails || got.CostCeiling {
		t.Errorf("unbounded session reports %+v, want guardrails wired and no ceiling", got)
	}

	cfg.GuardrailsFn = func() attach.GuardrailInfo {
		return attach.GuardrailInfo{CostCeiling: attach.CostCeilingInfo{MaxTurns: 40}}
	}
	bounded, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A turn cap is a ceiling: a dollars-only reading of Configured
	// would report "no cost guardrail" on a session that halts at 40
	// model calls.
	if got := bounded.AttachCapabilities(); !got.CostCeiling {
		t.Errorf("turn-capped session reports no cost ceiling: %+v", got)
	}
}

// Absence has to be distinguishable at the call site: the read answers
// zero-value (the handler renders 200), the write reports the sentinel
// the handler turns into 501.
func TestGuardrailCallsWithoutWiring(t *testing.T) {
	ad, err := New(baseConfig(t, func(context.Context, string) (TurnResult, error) {
		return TurnResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := ad.AttachGuardrails(); got.Halted {
		t.Errorf("unwired read = %+v, want zero value", got)
	}
	if _, err := ad.AttachResetGuardrail(attach.GuardrailResetRequest{}); !errors.Is(err, attach.ErrCapabilityNotRegistered) {
		t.Errorf("unwired reset error = %v, want ErrCapabilityNotRegistered", err)
	}
}

// The request the daemon services must be the one the operator sent —
// including the caller the handler stamped, which is the whole audit
// trail for "who handed this session more runway?".
func TestResetGuardrailPassesTheRequestThrough(t *testing.T) {
	var got attach.GuardrailResetRequest
	cfg := baseConfig(t, func(context.Context, string) (TurnResult, error) { return TurnResult{}, nil })
	cfg.ResetGuardrailFn = func(req attach.GuardrailResetRequest) (attach.GuardrailResetResponse, error) {
		got = req
		return attach.GuardrailResetResponse{Reset: []string{attach.GuardrailCostCeiling}}, nil
	}
	ad, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := attach.GuardrailResetRequest{
		Guardrail:        attach.GuardrailCostCeiling,
		AdditionalTurns:  10,
		AdditionalTokens: 5000,
		Scope:            "log-analyst",
		Caller:           "op@example.com",
	}
	resp, err := ad.AttachResetGuardrail(want)
	if err != nil {
		t.Fatalf("AttachResetGuardrail: %v", err)
	}
	if got != want {
		t.Errorf("daemon saw %+v, want %+v", got, want)
	}
	if len(resp.Reset) != 1 {
		t.Errorf("response not returned verbatim: %+v", resp)
	}
}

// A guardrail reset arrives mid-incident, which is exactly when a turn
// is running — it must not queue behind the turn it exists to unwedge.
func TestResetGuardrailRunsDuringATurn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cfg := baseConfig(t, func(context.Context, string) (TurnResult, error) {
		close(started)
		<-release
		return TurnResult{}, nil
	})
	cfg.ResetGuardrailFn = func(attach.GuardrailResetRequest) (attach.GuardrailResetResponse, error) {
		return attach.GuardrailResetResponse{Message: "raised"}, nil
	}
	ad, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.Inject("x"); err != nil {
		t.Fatal(err)
	}
	<-started
	done := make(chan error, 1)
	go func() {
		_, err := ad.AttachResetGuardrail(attach.GuardrailResetRequest{})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("reset during a turn: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("reset blocked behind the running turn")
	}
	close(release)
	// Let the turn land before the eventlog handle is torn down.
	deadline := time.Now().Add(5 * time.Second)
	for ad.AttachStatus().State != "idle" {
		if time.Now().After(deadline) {
			t.Fatal("turn never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestInterruptedTurnIsNotReportedAsRetryable is the wire-level half
// of the classifier change (#206). A turn stopped on purpose emits a
// turn-error frame like any other failure, and what a client keys its
// retry affordance off is that frame — so the interesting assertion is
// not what ClassifyTurnError returns in isolation but what the adapter
// actually puts on the stream after AttachInterrupt. It used to be
// transient_network / retryable:true, i.e. an invitation to re-run the
// work the operator had just stopped.
func TestInterruptedTurnIsNotReportedAsRetryable(t *testing.T) {
	started := make(chan struct{})
	ad, err := New(baseConfig(t, func(ctx context.Context, _ string) (TurnResult, error) {
		close(started)
		<-ctx.Done()
		return TurnResult{}, ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}

	frames := make(chan attach.TurnError, 1)
	ad.SetOperatorEventEmitter(func(eventType string, payload any) {
		if eventType != attach.EventTurnError {
			return
		}
		te, ok := payload.(attach.TurnError)
		if !ok {
			t.Errorf("turn-error payload is %T, want attach.TurnError", payload)
			return
		}
		select {
		case frames <- te:
		default:
		}
	})

	if err := ad.Inject("long turn"); err != nil {
		t.Fatal(err)
	}
	<-started
	if !ad.AttachInterrupt() {
		t.Fatal("AttachInterrupt with a turn in flight returned false")
	}

	select {
	case te := <-frames:
		if te.Kind != attach.TurnErrorCanceled {
			t.Errorf("kind = %q, want %q", te.Kind, attach.TurnErrorCanceled)
		}
		if te.Retryable {
			t.Error("an interrupted turn was advertised as retryable; a client would offer to re-run what the operator stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no turn-error frame after the interrupt")
	}
}

// The /usage digest_methods block: one of the two protocol surfaces the
// digest wrap was supposed to fill and did not until #221. It is
// decorated onto whatever UsageFn returns rather than produced by it,
// because pkg/digest's counters are process-global and no UsageFn has a
// session to scope them to.
//
// Not parallel, and resets around itself: the counters it reads are the
// process's.
func TestAttachUsageCarriesTheDigestBlock(t *testing.T) {
	digest.ResetTelemetry()
	t.Cleanup(digest.ResetTelemetry)

	run := func(context.Context, string) (TurnResult, error) { return TurnResult{}, nil }
	cfg := baseConfig(t, run)
	cfg.UsageFn = func() attach.UsageInfo { return attach.UsageInfo{Overall: attach.UsageTotals{InputTokens: 42}} }
	ad, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Nothing has digested yet — under --mcp-digest=false nothing ever
	// will, and the block must stay absent rather than report zeros.
	if got := ad.AttachUsage(); got.DigestMethods != nil {
		t.Errorf("digest_methods = %#v before any digest ran, want it omitted", got.DigestMethods)
	}

	item := `{"name":"pod","detail":"` + strings.Repeat("x", 200) + `"},`
	payload := []byte(`{"items":[` + strings.Repeat(item, 200) + `{"name":"last"}]}`)
	if _, err := digest.Process(context.Background(), payload, digest.Options{Threshold: 100}); err != nil {
		t.Fatalf("digest.Process: %v", err)
	}

	got := ad.AttachUsage()
	if got.Overall.InputTokens != 42 {
		t.Errorf("Overall.InputTokens = %d, want the UsageFn's 42 — decorating must not replace it", got.Overall.InputTokens)
	}
	if got.DigestMethods == nil {
		t.Fatal("digest_methods is absent after a digest ran; the block is unreachable")
	}
	if got.DigestMethods.Counts[digest.MethodStructuralJSON] != 1 {
		t.Errorf("counts = %v, want one structural_json", got.DigestMethods.Counts)
	}
	if got.DigestMethods.BytesSaved[digest.MethodStructuralJSON] <= 0 {
		t.Errorf("bytes_saved = %v, want a positive reduction", got.DigestMethods.BytesSaved)
	}
}
