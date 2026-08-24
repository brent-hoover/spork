package devloop_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/architect"
	"kriya/internal/devloop"
	"kriya/internal/fakes"
	"kriya/internal/reviewbridge"
)

func TestASecondSessionOnOneAttemptDoesNotReuseRoundIds(t *testing.T) {
	// An architect handoff ends the session and the next pass opens a fresh
	// one — at the SAME gate attempt, with Rounds back at zero. Round ids
	// built from (run, attempt, round) then repeat, and the review store
	// overwrites the first session's job while keeping its commit: a trail
	// pointing at the wrong work.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictFindings},
		findings: "- **Severity**: High",
	}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 2)
	sa := &recordingArchitect{direction: "extract the port"}
	loop.Architect = sa

	if _, err := loop.Work(context.Background(), request()); !errors.Is(err, devloop.ErrArchitectDirected) {
		t.Fatalf("first pass: %v", err)
	}
	firstIDs := append([]string{}, rev.submitted...)
	if len(firstIDs) == 0 {
		t.Fatal("the first session submitted nothing")
	}

	// The next pass: a NEW session, the same attempt.
	sa.found = true
	sa.resume = architect.Intervention{Direction: sa.direction}
	rev.round, rev.verdicts = 0, []string{reviewbridge.VerdictClean}
	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	for _, id := range rev.submitted[len(firstIDs):] {
		for _, earlier := range firstIDs {
			if id == earlier {
				t.Errorf("the second session reused round id %q", id)
			}
		}
	}
}

func TestRoundIdsAreStableWithinOneSession(t *testing.T) {
	// The control: an id that changed between a submit and its settle would
	// break the trail just as badly in the other direction.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictClean},
		findings: "- **Severity**: Low",
	}
	if _, err := pairLoop(store, ag, &fakeCommitter{}, rev, 5).
		Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(rev.settled) != 2 {
		t.Fatalf("settled %v", rev.settled)
	}
	for i, id := range rev.settled {
		if id != rev.submitted[i] {
			t.Errorf("round %d was submitted as %q and settled as %q", i, rev.submitted[i], id)
		}
	}
}

func TestAFailedAgentDoesNotCloseTheIntervention(t *testing.T) {
	// MarkResumed ran before the invocation. An agent that then failed left
	// the intervention permanently resumed with its direction delivered to
	// nobody — and a retry could no longer retrieve it.
	store := &memStore{}
	ag := &fakes.Agent{Err: errors.New("claude unavailable")}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictClean}}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)
	sa := &recordingArchitect{
		found:  true,
		resume: architect.Intervention{Direction: "extract the port"},
	}
	loop.Architect = sa

	if _, err := loop.Work(context.Background(), request()); err == nil {
		t.Fatal("a failed agent read as a completed session")
	}
	if sa.delivered != 0 {
		t.Errorf("the intervention was closed %d times for a direction nobody received",
			sa.delivered)
	}
}
