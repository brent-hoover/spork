package main

import (
	"testing"

	"kriya/internal/orchestrator"
	"kriya/internal/planner"
)

func TestDetectionRefusesATargetNobodyPlanned(t *testing.T) {
	// Not armed, and not a stall either: the operator has not started this
	// build, and an inbox item for it would be noise.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	armed, reason, err := completionDetector(db).Detect(t.Context(), "/never-planned")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if armed {
		t.Error("a target with no build target row armed")
	}
	if reason != "" {
		t.Errorf("an unstarted build reported a stall cause: %q", reason)
	}
}

func TestTheStallRecorderWritesThroughToTheInbox(t *testing.T) {
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := stallRecorder(db).Record(t.Context(), "/spec", 3, "nothing workable"); err != nil {
		t.Fatalf("record: %v", err)
	}
	open, err := (orchestrator.SQLStalls{DB: db}).Open(t.Context())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(open) != 1 || open[0].Cause != "nothing workable" {
		t.Errorf("the inbox holds %+v", open)
	}
}

func TestAClaimResolvesItsProjectFromTheTargetsOwnRow(t *testing.T) {
	// From the ROW, never the caller: a replay must reproduce the original
	// request, and a project re-derived elsewhere is a second place for the
	// two to disagree.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLTargets{DB: db}).Upsert(t.Context(), planner.BuildTarget{
		TargetKey: "/spec", ProjectID: "p-1", EpicID: "epic-1",
		EpicState: planner.EpicCreated,
	}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	project, epic, err := targetsFor(db)(planner.CompletionClaim{TargetKey: "/spec"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if project != "p-1" || epic != "epic-1" {
		t.Errorf("resolved project %q epic %q", project, epic)
	}
	// A claim for a target that is not there is a defect, not a silent skip:
	// replaying it would submit against nothing.
	if _, _, err := targetsFor(db)(planner.CompletionClaim{TargetKey: "/gone"}); err == nil {
		t.Error("a claim for an absent target resolved anyway")
	}
}

func TestAnUnarmedTargetSubmitsNoCompletionReview(t *testing.T) {
	// The driver asks before it claims. A target still holding open work must
	// not reach sutra at all — the tracker is unreachable in this test, so a
	// submission would fail loudly.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLPlans{DB: db}).Upsert(t.Context(), planner.Plan{
		TargetKey: "/spec", SpecHash: "h", State: planner.PlanDecomposing,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if err := claimCompletion(t.Context(), db, "/spec", "actor-1"); err != nil {
		t.Errorf("an unarmed target reached the tracker: %v", err)
	}
	if _, found, _ := (planner.SQLClaims{DB: db}).Find(t.Context(), "/spec"); found {
		t.Error("an unarmed target recorded a completion claim")
	}
}

func TestAClaimAlreadyInFlightIsNotSubmittedTwice(t *testing.T) {
	// A second submission for one claim would open a second review, and the
	// epoch-scoped key would return the first — so the two would disagree
	// about which review a human is looking at.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLClaims{DB: db}).Upsert(t.Context(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionSubmitted,
		SubmissionKey: "sub-1", ReviewID: "review-1",
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	// The tracker is unreachable; reaching it would fail. Nothing does.
	if err := claimCompletion(t.Context(), db, "/spec", "actor-1"); err != nil {
		t.Errorf("a settled claim was submitted again: %v", err)
	}
	got, _, _ := (planner.SQLClaims{DB: db}).Find(t.Context(), "/spec")
	if got.ReviewID != "review-1" {
		t.Errorf("the claim was replaced: %+v", got)
	}
}
