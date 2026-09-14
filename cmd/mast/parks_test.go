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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/approval"
	"github.com/go-steer/mast/pkg/inject"
	"github.com/go-steer/mast/pkg/transcript"
)

// The end-to-end claim #314 makes, read out of a real store: a parked
// session's change set reaches the wire, and the transcript around it
// does not.

// parkStore seeds sessions and returns a reader over them. The
// projection under test is the mapping between two vocabularies, so the
// store is the real one — a stand-in would only assert the mapping
// against itself.
func parkStore(t *testing.T) (*transcript.Store, adksession.Service) {
	t.Helper()
	svc := adksession.InMemoryService()
	return transcript.NewStore(svc, appName), svc
}

func seedParkSession(t *testing.T, svc adksession.Service, sid string, events ...*adksession.Event) {
	t.Helper()
	seedParkSessionAt(t, svc, sid, time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC), events...)
}

// seedParkSessionAt is seedParkSession with the event clock supplied,
// which is what lets a test make the store's own ordering disagree with
// the one the park list promises.
func seedParkSessionAt(t *testing.T, svc adksession.Service, sid string, base time.Time, events ...*adksession.Event) {
	t.Helper()
	ctx := context.Background()
	resp, err := svc.Create(ctx, &adksession.CreateRequest{AppName: appName, UserID: "operator", SessionID: sid})
	if err != nil {
		t.Fatalf("create session %q: %v", sid, err)
	}
	for i, ev := range events {
		ev.Timestamp = base.Add(time.Duration(i+1) * time.Second)
		if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("append event %d to %q: %v", i, sid, err)
		}
	}
}

// gatePark builds what the write gate leaves in the log when it parks a
// mutating call: a long-running adk_request_confirmation whose args
// carry the original call and the gate's own Request payload.
func gatePark(callID string, req approval.Request) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = "remediator"
	ev.LongRunningToolIDs = []string{callID}
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{
			ID:   callID,
			Name: toolconfirmation.FunctionCallName,
			Args: map[string]any{
				"originalFunctionCall": &genai.FunctionCall{ID: callID, Name: req.Tool, Args: req.Args},
				"toolConfirmation":     &toolconfirmation.ToolConfirmation{Hint: "Approve " + req.Key + "?", Payload: req},
			},
		},
	}}}
	return ev
}

// questionPark is the other kind: the agent asking, with nothing to
// authorize.
func questionPark(callID, question string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = "triager"
	ev.LongRunningToolIDs = []string{callID}
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{
			ID: callID, Name: "request_operator_input",
			Args: map[string]any{"message": question},
		},
	}}}
	return ev
}

func modelChatter(text string) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = "remediator"
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	return ev
}

func captureEvent(r approval.CaptureRecord) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = "remediator"
	ev.Content = genai.NewContentFromText("acting", genai.RoleModel)
	raw, err := approval.EncodeCapture(r)
	if err != nil {
		panic("encode capture: " + err.Error())
	}
	ev.Actions.StateDelta = map[string]any{approval.CaptureStateKey(r.FunctionCallID): raw}
	return ev
}

func scaleRequest() approval.Request {
	return approval.Request{
		Tool:    "scale_deployment",
		Args:    map[string]any{"deployment": "api", "replicas": float64(10)},
		Key:     "scale_deployment(deployment=api, replicas=10)",
		Policy:  "ask",
		Agent:   "remediator",
		Verdict: map[string]any{"verdict": "approve | reject | edit"},
		Stale:   "the Deployment changed since you approved this",
		ChangeSet: &approval.ChangeSetContext{
			Specialist: "remediator",
			Changes: []approval.ProposedChange{
				{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api", "replicas": float64(10)}},
				{Tool: "restart_deployment", Arguments: map[string]any{"deployment": "api"}},
			},
			Grantable:     true,
			TTLSeconds:    600,
			Preconditions: map[string]string{"scale_deployment": "get_deployment(name=api)"},
		},
	}
}

func readParks(t *testing.T, store *transcript.Store, sid string) inject.ParksResult {
	t.Helper()
	out, err := parksHandler(store, nil)(context.Background(), inject.ParksRequest{SessionID: sid})
	if err != nil {
		t.Fatalf("park read (%q): %v", sid, err)
	}
	return out
}

// The change set reaches the wire intact. This is what an in-chat
// approval renders, and the reason a button that cannot read it is
// uninformed consent rather than an approval.
func TestAParkedChangeSetReachesTheWire(t *testing.T) {
	t.Parallel()
	store, svc := parkStore(t)
	seedParkSession(t, svc, "incident-1",
		modelChatter("I think the API deployment is under-provisioned."),
		gatePark("call-1", scaleRequest()),
	)

	got := readParks(t, store, "incident-1")
	if len(got.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(got.Sessions))
	}
	s := got.Sessions[0]
	if s.SessionID != "incident-1" || s.State != transcript.StatePaused {
		t.Errorf("session = %q %q, want incident-1 paused", s.SessionID, s.State)
	}
	if s.Awaiting != inject.ParkKindApproval {
		t.Errorf("awaiting = %q, want %q", s.Awaiting, inject.ParkKindApproval)
	}
	if len(s.Parks) != 1 {
		t.Fatalf("parks = %d, want 1", len(s.Parks))
	}
	p := s.Parks[0]
	if p.Kind != inject.ParkKindApproval {
		t.Errorf("kind = %q, want %q", p.Kind, inject.ParkKindApproval)
	}
	if p.InterruptID != "call-1" {
		t.Errorf("interrupt_id = %q, want call-1 — the ID a resume has to quote", p.InterruptID)
	}
	if p.Author != "remediator" || p.RaisedAt.IsZero() {
		t.Errorf("park = %+v, want an author and a raised-at", p)
	}
	if p.Change == nil {
		t.Fatal("the park carries no change; an approval surface has nothing to render but the word Approve")
	}
	c := p.Change
	if c.Tool != "scale_deployment" || c.Key != "scale_deployment(deployment=api, replicas=10)" {
		t.Errorf("change = %q / %q, want the scale call", c.Tool, c.Key)
	}
	if got := c.Args["replicas"]; got != float64(10) {
		t.Errorf("args[replicas] = %v (%T), want 10 — the number the operator is authorizing", got, got)
	}
	if c.Policy != "ask" || c.Agent != "remediator" {
		t.Errorf("policy/agent = %q/%q, want ask/remediator", c.Policy, c.Agent)
	}
	if c.Stale == "" {
		t.Error("stale is empty; the difference between 'approve this' and 'you approved this and the ground moved' is gone")
	}
	if c.VerdictFormat["verdict"] != "approve | reject | edit" {
		t.Errorf("verdict_format = %v, want the three-valued vocabulary", c.VerdictFormat)
	}
	if c.ChangeSet == nil {
		t.Fatal("the change set is absent, so `scope: change_set` is on the table with nothing shown")
	}
	if n := len(c.ChangeSet.Changes); n != 2 {
		t.Fatalf("change set = %d calls, want 2", n)
	}
	if c.ChangeSet.Changes[1].Tool != "restart_deployment" {
		t.Errorf("second call = %q, want restart_deployment", c.ChangeSet.Changes[1].Tool)
	}
	if !c.ChangeSet.Grantable || c.ChangeSet.TTLSeconds != 600 {
		t.Errorf("grantable/ttl = %v/%d, want true/600", c.ChangeSet.Grantable, c.ChangeSet.TTLSeconds)
	}
	if c.ChangeSet.Preconditions["scale_deployment"] != "get_deployment(name=api)" {
		t.Errorf("preconditions = %v, want the freshness read named", c.ChangeSet.Preconditions)
	}
}

// #314's third Done-when, as an assertion rather than a paragraph: the
// projection carries no model output and no tool results.
func TestTheParkProjectionLeavesTheTranscriptBehind(t *testing.T) {
	t.Parallel()
	store, svc := parkStore(t)
	const secret = "the cluster's admin password is hunter2"
	rec := scaleCaptureRecord()
	rec.Prior = map[string]any{"spec.replicas": float64(3), "note": secret}
	seedParkSession(t, svc, "incident-2",
		modelChatter("Chain of thought: "+secret),
		captureEvent(rec),
		gatePark("call-1", scaleRequest()),
	)

	got := readParks(t, store, "incident-2")
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Errorf("the park projection carries transcript content:\n%s", raw)
	}
	if strings.Contains(string(raw), "Chain of thought") {
		t.Errorf("the park projection carries model reasoning:\n%s", raw)
	}

	// The capture itself IS carried — the undo is the point (#296) —
	// minus the captured values.
	s := got.Sessions[0]
	if len(s.Applied) != 1 {
		t.Fatalf("applied = %d, want 1: the revert is what makes this readable at 3am", len(s.Applied))
	}
	a := s.Applied[0]
	if a.Revert == nil || a.Revert.Args["replicas"] != float64(3) {
		t.Fatalf("revert = %+v, want the call that puts 3 replicas back", a.Revert)
	}
	if a.Digest == "" || a.Read == "" || len(a.PriorFields) == 0 {
		t.Errorf("applied = %+v, want the digest, the read and the narrowed fields — enough to take the read again", a)
	}
	if a.CallID != "fc-1" {
		t.Errorf("call_id = %q, want fc-1 — the join key to the decision record", a.CallID)
	}
}

// A question is not a change. Rendering an Approve button on one asks
// an operator to authorize a sentence.
func TestAQuestionParkCarriesNoChange(t *testing.T) {
	t.Parallel()
	store, svc := parkStore(t)
	seedParkSession(t, svc, "incident-3", questionPark("call-9", "Which namespace should I look at?"))

	s := readParks(t, store, "incident-3").Sessions[0]
	if s.Awaiting != inject.ParkKindInput {
		t.Errorf("awaiting = %q, want %q", s.Awaiting, inject.ParkKindInput)
	}
	if len(s.Parks) != 1 || s.Parks[0].Kind != inject.ParkKindInput {
		t.Fatalf("parks = %+v, want one input park", s.Parks)
	}
	if s.Parks[0].Change != nil {
		t.Errorf("a question carries a change: %+v", s.Parks[0].Change)
	}
	if s.Parks[0].Question != "Which namespace should I look at?" {
		t.Errorf("question = %q, want the text the agent asked", s.Parks[0].Question)
	}
}

// An approval outranks a question at the session level, the same
// ranking #313 put on turn_state — but BOTH parks are listed, because
// the ranking is about which prompt to lead with, not which question
// exists.
func TestAnApprovalOutranksAQuestionWithoutHidingIt(t *testing.T) {
	t.Parallel()
	store, svc := parkStore(t)
	seedParkSession(t, svc, "incident-4",
		questionPark("call-8", "Which namespace?"),
		gatePark("call-9", scaleRequest()),
	)

	s := readParks(t, store, "incident-4").Sessions[0]
	if s.Awaiting != inject.ParkKindApproval {
		t.Errorf("awaiting = %q, want %q", s.Awaiting, inject.ParkKindApproval)
	}
	if len(s.Parks) != 2 {
		t.Fatalf("parks = %d, want both", len(s.Parks))
	}
	kinds := map[string]bool{}
	for _, p := range s.Parks {
		kinds[p.Kind] = true
	}
	if !kinds[inject.ParkKindApproval] || !kinds[inject.ParkKindInput] {
		t.Errorf("parks = %v, want one of each kind", kinds)
	}
}

// A hold is not a park. It is reported so a client can say why the
// session is not moving, and it is not in the list of things anybody
// can answer.
func TestAHoldIsReportedBesideTheParksAndNotAsOne(t *testing.T) {
	t.Parallel()
	store, svc := parkStore(t)
	seedParkSession(t, svc, "incident-5", modelChatter("looking into it"))
	if _, _, err := store.PauseGate(context.Background(), "operator", "incident-5", transcript.PauseSpec{
		Reason: transcript.ReasonOperator, Message: "paging the on-call",
	}); err != nil {
		t.Fatalf("pause gate: %v", err)
	}

	s := readParks(t, store, "incident-5").Sessions[0]
	if s.State != transcript.StatePaused {
		t.Errorf("state = %q, want paused", s.State)
	}
	if len(s.Parks) != 0 {
		t.Errorf("a hold produced %d parks; nothing resolves by answering it", len(s.Parks))
	}
	if s.Awaiting != "" {
		t.Errorf("awaiting = %q, want empty — a hold is not a question anyone owes an answer to", s.Awaiting)
	}
	if s.Hold == nil {
		t.Fatal("the hold is absent, so a client can only report a paused session with no reason")
	}
	if s.Hold.Message != "paging the on-call" || s.Hold.Token == "" || s.Hold.ExpiresAt.IsZero() {
		t.Errorf("hold = %+v, want the reason, the token and its expiry", s.Hold)
	}
}

// The list route: parked sessions only, oldest question first.
//
// The two orderings are deliberately made to disagree. transcript.List
// answers most-recent-event-first, which would put the freshest
// question at the top of an approval queue; the one that has been
// waiting longest is the one whose answer is most overdue.
func TestTheListRouteReturnsParkedSessionsOldestFirst(t *testing.T) {
	t.Parallel()
	store, svc := parkStore(t)
	day := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seedParkSessionAt(t, svc, "waiting-since-breakfast", day, questionPark("call-2", "which namespace?"))
	seedParkSessionAt(t, svc, "just-parked", day.Add(2*time.Hour), gatePark("call-1", scaleRequest()))
	seedParkSessionAt(t, svc, "quiet", day.Add(time.Hour), modelChatter("all done"))

	out, err := parksHandler(store, nil)(context.Background(), inject.ParksRequest{})
	if err != nil {
		t.Fatalf("park list: %v", err)
	}
	var ids []string
	for _, s := range out.Sessions {
		ids = append(ids, s.SessionID)
	}
	if len(ids) != 2 {
		t.Fatalf("listed %v, want only the two parked sessions", ids)
	}
	for _, id := range ids {
		if id == "quiet" {
			t.Error("the list includes a session with nothing to answer")
		}
	}
	if ids[0] != "waiting-since-breakfast" || ids[1] != "just-parked" {
		t.Errorf("order = %v, want the oldest question first — this is the store's own order, not the queue's", ids)
	}
}

// A session that exists and is not parked answers with an empty list, a
// named session that does not exist answers ErrNotFound. A client
// polling "is my approval still open" has to be able to tell those
// apart, and a 404 for the first one would read as "your session is
// gone".
func TestNotParkedAndNotFoundAreDifferentAnswers(t *testing.T) {
	t.Parallel()
	store, svc := parkStore(t)
	seedParkSession(t, svc, "incident-6", modelChatter("finished"))

	got := readParks(t, store, "incident-6")
	if len(got.Sessions) != 1 {
		t.Fatalf("sessions = %d, want the session itself", len(got.Sessions))
	}
	if len(got.Sessions[0].Parks) != 0 || got.Sessions[0].Awaiting != "" {
		t.Errorf("a finished session reports %+v, want nothing pending", got.Sessions[0])
	}

	_, err := parksHandler(store, nil)(context.Background(), inject.ParksRequest{SessionID: "no-such-session"})
	if !errors.Is(err, inject.ErrNotFound) {
		t.Errorf("unknown session → %v, want inject.ErrNotFound (the route's 404)", err)
	}
}

// A park whose payload mast cannot decode is still a park. Dropping it
// would hide a session that is waiting on somebody — the exact failure
// #313 just fixed on the attach side.
func TestAnUndecodableParkIsStillReported(t *testing.T) {
	t.Parallel()
	store, svc := parkStore(t)
	ev := adksession.NewEvent(context.Background(), "inv-1")
	ev.Author = "remediator"
	ev.LongRunningToolIDs = []string{"call-x"}
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{
			ID: "call-x", Name: toolconfirmation.FunctionCallName,
			// A confirmation raised by something other than mast's write
			// gate: no payload the gate would recognise.
			Args: map[string]any{"toolConfirmation": map[string]any{"hint": "Really?"}},
		},
	}}}
	seedParkSession(t, svc, "incident-7", ev)

	s := readParks(t, store, "incident-7").Sessions[0]
	if len(s.Parks) != 1 {
		t.Fatalf("parks = %d, want the undecodable park reported anyway", len(s.Parks))
	}
	if s.Parks[0].Kind != inject.ParkKindApproval {
		t.Errorf("kind = %q, want %q — it is still a confirmation", s.Parks[0].Kind, inject.ParkKindApproval)
	}
	if s.Parks[0].Question == "" {
		t.Error("the park carries no question at all, so an operator sees an unexplained pause")
	}
	if s.Parks[0].Change != nil {
		t.Errorf("change = %+v, want nil: a guessed reconstruction of a call is worse than a blank", s.Parks[0].Change)
	}
}

// A daemon with no store has no park read. The route answers 404, which
// is not the same claim as an empty list.
func TestNoStoreMeansNoHandler(t *testing.T) {
	t.Parallel()
	if parksHandler(nil, nil) != nil {
		t.Error("a daemon with no session store installed a park handler; it would answer 200 with an empty list")
	}
}

// parkKind is the seam between two vocabularies that spell the same two
// things identically today. The switch is what keeps that a fact: this
// pins both directions and the fallback, so a rename on either side
// lands here rather than in a client repo (#248).
func TestParkKindMapsTheTranscriptVocabulary(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		transcript.AwaitingApproval: inject.ParkKindApproval,
		transcript.AwaitingInput:    inject.ParkKindInput,
		"":                          "",
		"awaiting_permission":       "",
		"APPROVAL":                  "",
	} {
		if got := parkKind(in); got != want {
			t.Errorf("parkKind(%q) = %q, want %q", in, got, want)
		}
	}
}
