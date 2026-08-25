package orchestrator_test

import (
	"context"
	"testing"

	"kriya/internal/orchestrator"
)

func spikeVerdictRouter(
	verdict string, run orchestrator.BuildRun, store *memStore,
) (orchestrator.Router, *sessionRoutes) {
	// A finding review carries NO SESSION. sutra's verdict payload has one
	// field for it and a document review never fills it.
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Review: "review-1", Verdict: verdict,
		}}},
		next: map[string]string{"": "c-1"},
	}
	routes := &sessionRoutes{
		bySession: map[string]orchestrator.BuildRun{},
		byReview:  map[string]orchestrator.BuildRun{"review-1": run},
	}
	return router(feed, newMemCursors(), routes, store), routes
}

func spikeAwaitingVerdict() orchestrator.BuildRun {
	return orchestrator.BuildRun{
		ID: "run-spike", Ticket: "spike: headless?", Issue: "issue-7",
		Kind: orchestrator.KindSpike, State: orchestrator.StateFindingSubmitted,
		ReviewID: "review-1", ReviewRevision: 1, FindingVersion: "ver-1",
	}
}

func TestAFindingVerdictIsRoutedByItsReview(t *testing.T) {
	// It carries no session: there was no pair loop and no agent instance to
	// route feedback back to. Routing by session alone loses the verdict
	// entirely, and the spike waits forever on an answer that already came.
	store := newMemStore()
	r, _ := spikeVerdictRouter(orchestrator.VerdictApproved, spikeAwaitingVerdict(), store)
	routed, _, err := r.Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(routed) != 1 {
		t.Fatalf("routed %d verdicts for a sessionless review", len(routed))
	}
	if routed[0].Run.ID != "run-spike" {
		t.Errorf("routed to %q", routed[0].Run.ID)
	}
	if !routed[0].Spike {
		t.Error("the verdict was not recognised as a finding's")
	}
}

func TestARejectedFindingReturnsToTheResearchLoop(t *testing.T) {
	// NOT the dev loop. There is no code to fix: the agent is being asked to
	// answer the same risk better, and the pair loop would have it write an
	// implementation nothing gates.
	store := newMemStore()
	r, _ := spikeVerdictRouter(orchestrator.VerdictChangesRequested,
		spikeAwaitingVerdict(), store)
	routed, _, err := r.Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(routed) != 1 || !routed[0].Reworked {
		t.Fatalf("routed %+v", routed)
	}
	if routed[0].Run.State != orchestrator.StateResearchLoop {
		t.Errorf("a rejected finding went to %q", routed[0].Run.State)
	}
	// And the verdict it must answer is on the run, durably.
	got := store.rows["run-spike"]
	if got.ReviewVerdictEvent != "event-9" {
		t.Errorf("the run records verdict event %q", got.ReviewVerdictEvent)
	}
	if got.State != orchestrator.StateResearchLoop {
		t.Errorf("the durable state is %q", got.State)
	}
}

func TestARejectedCodeReviewStillReturnsToTheDevLoop(t *testing.T) {
	// The control: routing findings to the research loop must not reroute
	// code work away from the pair loop.
	store := newMemStore()
	run := submittedFor("sess-42")
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Review: "review-1", Session: "sess-42",
			Verdict: orchestrator.VerdictChangesRequested,
		}}},
		next: map[string]string{"": "c-1"},
	}
	routes := &sessionRoutes{
		bySession: map[string]orchestrator.BuildRun{"sess-42": run},
	}
	routed, _, err := router(feed, newMemCursors(), routes, store).
		Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(routed) != 1 {
		t.Fatalf("routed %d", len(routed))
	}
	if routed[0].Run.State != orchestrator.StateDevLoop {
		t.Errorf("a rejected code review went to %q", routed[0].Run.State)
	}
	if routed[0].Spike {
		t.Error("a code review was routed as a finding")
	}
}

func TestASessionedVerdictPrefersItsSession(t *testing.T) {
	// The session is what identifies the agent instance that wrote the code,
	// which is where feedback belongs. Falling back to the review first would
	// be right often enough to hide the difference and wrong when a run's
	// review id has been rotated.
	store := newMemStore()
	bySession := submittedFor("sess-42")
	byReview := spikeAwaitingVerdict()
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Review: "review-1", Session: "sess-42",
			Verdict: orchestrator.VerdictApproved,
		}}},
		next: map[string]string{"": "c-1"},
	}
	routes := &sessionRoutes{
		bySession: map[string]orchestrator.BuildRun{"sess-42": bySession},
		byReview:  map[string]orchestrator.BuildRun{"review-1": byReview},
	}
	routed, _, err := router(feed, newMemCursors(), routes, store).
		Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if routed[0].Run.ID != bySession.ID {
		t.Errorf("routed to %q, not the session's run", routed[0].Run.ID)
	}
}

func TestAVerdictForNothingKriyaKnowsIsSkipped(t *testing.T) {
	// The feed is shared. Another consumer's reviews are none of kriya's
	// business, and neither handle finding one is an error.
	store := newMemStore()
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Review: "review-foreign",
			Verdict: orchestrator.VerdictApproved,
		}}},
		next: map[string]string{"": "c-1"},
	}
	routed, _, err := router(feed, newMemCursors(), &sessionRoutes{}, store).
		Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(routed) != 0 {
		t.Errorf("routed %+v for a review nothing owns", routed)
	}
}
