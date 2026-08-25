package main

import (
	"testing"

	"kriya/internal/planner"
)

func TestTheClaimerIsWiredWithEveryCollaborator(t *testing.T) {
	// A WIRING test. Every seam the claim protocol drives has been the site
	// of a bug found by review rather than by a unit test: the module was
	// right and the composition root did not call it. A nil collaborator
	// panics at the worst possible moment — mid-protocol, after a write-ahead
	// row promised a call that can no longer happen.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	c := completionClaimer(db, "actor-1", "/spec")
	for name, wired := range map[string]bool{
		// Without it, every completion event the build itself emitted reads
		// as a reopen the moment the claim exists.
		"work":      c.Work != nil,
		"claims":    c.Claims != nil,
		"reports":   c.Reports != nil,
		"documents": c.Documents != nil,
		"reviews":   c.Reviews != nil,
		"epics":     c.Epics != nil,
		"epochs":    c.Epochs.Store != nil,
	} {
		if !wired {
			t.Errorf("the claimer has no %s", name)
		}
	}
}

func TestAnUnsubmittedTargetObservesNoApproval(t *testing.T) {
	// The driver looks for an approval only on a claim resting at
	// review-submitted. Anything else must not reach the tracker — which is
	// unreachable here, so a call would fail loudly.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := observeCompletionApproval(t.Context(), db, "/never-claimed", "actor-1"); err != nil {
		t.Errorf("a target with no claim reached the tracker: %v", err)
	}
	// A claim still mid-submission is not awaiting a human yet.
	if err := (planner.SQLClaims{DB: db}).Upsert(t.Context(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionSubmitting, SubmissionKey: "k",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := observeCompletionApproval(t.Context(), db, "/spec", "actor-1"); err != nil {
		t.Errorf("a submitting claim reached the tracker: %v", err)
	}
	// And one already complete is finished.
	if err := (planner.SQLClaims{DB: db}).Upsert(t.Context(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionComplete, ReviewID: "review-1",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := observeCompletionApproval(t.Context(), db, "/spec", "actor-1"); err != nil {
		t.Errorf("a completed claim reached the tracker: %v", err)
	}
}

func TestTheCloseColumnsSurviveTheRoundTrip(t *testing.T) {
	// The close key and its approval event are what recovery replays under.
	// They arrived in a later migration than the rest of the claim, so a
	// column the upsert forgot would be silently blank on every read.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := planner.SQLClaims{DB: db}
	want := planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionClosing, Epoch: 2,
		SubmissionKey: "sub", ReportKey: "doc", PendingReport: "# Done",
		ReportDoc: "doc-1", ReportVersion: "ver-1", ReviewID: "review-1",
		ReviewRevision: 2, SubtreeRevision: 7,
		ApprovalEvent: "event-9", CloseKey: "close-key",
	}
	if err := s.Upsert(t.Context(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.Find(t.Context(), "/spec")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got != want {
		t.Errorf("read back %+v", got)
	}
	closing, err := s.Closing(t.Context())
	if err != nil {
		t.Fatalf("closing: %v", err)
	}
	if len(closing) != 1 || closing[0].CloseKey != "close-key" {
		t.Errorf("recovery would replay %+v", closing)
	}
}
