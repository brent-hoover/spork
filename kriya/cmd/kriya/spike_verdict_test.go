package main

import (
	"testing"

	"kriya/internal/agent"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
	"kriya/internal/reviewbridge"
	"kriya/internal/workspace"
)

func TestAnApprovedFindingRetiresRatherThanMerging(t *testing.T) {
	// A spike's deliverable is a document. Sending its approval to the merge
	// queue would try to land a finding on a branch — and the queue would
	// preflight a commit that does not exist.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	spike := orchestrator.BuildRun{
		ID: "run-spike", Ticket: "spike: headless?", Issue: "issue-7",
		Plan: "/target", Kind: orchestrator.KindSpike,
		State: orchestrator.StateFindingSubmitted, ReviewID: "review-1",
		ReviewRevision: 1, FindingVersion: "ver-1",
	}
	if err := (orchestrator.SQLStore{DB: db}).Upsert(t.Context(), spike); err != nil {
		t.Fatalf("seed: %v", err)
	}
	queue := queueForTest(t, db)

	// The tracker is unreachable, so the retirement fails — which is what
	// proves the ATTEMPT was a retirement and not an enqueue.
	_ = actOnVerdicts(t.Context(), db, workspace.Manager{}, agent.Tiers{},
		reviewbridge.Bridge{}, queue, []orchestrator.Routed{{
			Run: spike, Revision: 1, Spike: true,
			Verdict: orchestrator.VerdictEvent{
				ID: "event-9", Review: "review-1",
				Verdict: orchestrator.VerdictApproved,
			},
		}}, "/target", "actor-1")

	if _, found, err := queue.AttemptFor(t.Context(), "run-spike"); err != nil {
		t.Fatalf("attempt for: %v", err)
	} else if found {
		t.Error("a finding was enqueued for merging")
	}
	// The write-ahead close row is what says a retirement was attempted.
	got, _, err := (orchestrator.SQLStore{DB: db}).Find(t.Context(), "run-spike")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.CloseKey == "" {
		t.Error("no retirement was attempted")
	}
}

func TestAnApprovedCodeReviewStillEnqueues(t *testing.T) {
	// The control: routing findings away from the merge queue must not route
	// code away from it.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	run := orchestrator.BuildRun{
		ID: "run-code", Ticket: "T-1", Issue: "issue-8", Plan: "/target",
		State: orchestrator.StateReviewSubmitted, ReviewID: "review-2",
		ReviewCommit: "C2", Branch: "kriya/T-1/abcd",
	}
	if err := (orchestrator.SQLStore{DB: db}).Upsert(t.Context(), run); err != nil {
		t.Fatalf("seed: %v", err)
	}
	queue := queueForTest(t, db)
	_ = actOnVerdicts(t.Context(), db, workspace.Manager{}, agent.Tiers{},
		reviewbridge.Bridge{}, queue, []orchestrator.Routed{{
			Run: run, Revision: 1,
			Verdict: orchestrator.VerdictEvent{
				ID: "event-9", Review: "review-2", Session: "sess-1",
				Verdict: orchestrator.VerdictApproved,
			},
		}}, "/target", "actor-1")

	if _, found, err := queue.AttemptFor(t.Context(), "run-code"); err != nil {
		t.Fatalf("attempt for: %v", err)
	} else if !found {
		t.Error("an approved code review was not enqueued for merging")
	}
}

func TestAFindingVerdictRoutesByReviewWithNoSession(t *testing.T) {
	// A finding review carries no session — there was no pair loop and no
	// agent instance to route feedback back to — so the review id is the only
	// handle its verdict has. Routing by session alone loses it entirely.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (orchestrator.SQLStore{DB: db}).Upsert(t.Context(), orchestrator.BuildRun{
		ID: "run-spike", Ticket: "spike", Issue: "issue-7", Plan: "/target",
		Kind: orchestrator.KindSpike, ReviewID: "review-1",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, found, err := (orchestrator.SQLStore{DB: db}).ByReview(t.Context(), "review-1")
	if err != nil {
		t.Fatalf("by review: %v", err)
	}
	if !found || got.ID != "run-spike" {
		t.Errorf("routed to %+v", got)
	}
	if _, found, _ := (orchestrator.SQLStore{DB: db}).ByReview(t.Context(), "review-absent"); found {
		t.Error("a review nothing owns was routed anyway")
	}
}

func TestTheResearcherIsWiredWithEveryCollaborator(t *testing.T) {
	// A WIRING test. Without Tickets an approved finding cannot close its
	// spike, so the risk never retires and everything behind it stays blocked.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	r := researcherFor(db, agent.Tiers{}, "/target", "actor-1")
	for name, wired := range map[string]bool{
		"store":    r.Store != nil,
		"findings": r.Findings != nil,
		"docs":     r.Docs != nil,
		"reviews":  r.Reviews != nil,
		"tickets":  r.Tickets != nil,
	} {
		if !wired {
			t.Errorf("the researcher has no %s", name)
		}
	}
	_ = planner.Ticket{}
}
