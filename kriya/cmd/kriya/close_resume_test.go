package main

import (
	"testing"

	"kriya/internal/planner"
)

func TestAClaimStrandedInClosingIsResumed(t *testing.T) {
	// Close persists CompletionClosing before calling sutra. Approval polling
	// accepted only review-submitted, so any close error left the claim
	// permanently stranded: normal driving neither retried the persisted key
	// nor observed a reapproval under a new verdict event.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLClaims{DB: db}).Upsert(t.Context(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionClosing, ReviewID: "review-1",
		ApprovalEvent: "event-9", CloseKey: "close-key",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	claim, _, err := (planner.SQLClaims{DB: db}).Find(t.Context(), "/spec")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !awaitsClose(claim) {
		t.Error("a claim stranded in closing is not resumed by the driver")
	}
}

func TestASubmittedClaimAlsoAwaitsItsClose(t *testing.T) {
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLClaims{DB: db}).Upsert(t.Context(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionSubmitted, ReviewID: "review-1",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	claim, _, err := (planner.SQLClaims{DB: db}).Find(t.Context(), "/spec")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !awaitsClose(claim) {
		t.Error("a submitted claim is not polled for its approval")
	}
}

func TestASettledClaimAwaitsNothing(t *testing.T) {
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, state := range []string{
		planner.CompletionComplete, planner.CompletionStale, planner.CompletionSubmitting,
	} {
		if err := (planner.SQLClaims{DB: db}).Upsert(t.Context(), planner.CompletionClaim{
			TargetKey: "/spec", State: state, ReviewID: "review-1",
		}); err != nil {
			t.Fatalf("seed %s: %v", state, err)
		}
		claim, _, err := (planner.SQLClaims{DB: db}).Find(t.Context(), "/spec")
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		if awaitsClose(claim) {
			t.Errorf("a claim in %q was polled for an approval", state)
		}
	}
	if awaitsClose(planner.CompletionClaim{}) {
		t.Error("a target with no claim was polled")
	}
}

func TestDetectionCarriesTheEpochAClaimBindsTo(t *testing.T) {
	// A WIRING test. The claim must bind to the epoch the DETECTION was
	// about: re-reading it at submission time would bind the attempt to an
	// epoch nothing checked for completion, so work that advanced it in
	// between would be covered by a review nobody looked at it for.
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
	if err := (planner.SQLPlans{DB: db}).Upsert(t.Context(), planner.Plan{
		TargetKey: "/spec", SpecHash: "h", State: planner.PlanCompleted, Tickets: 1,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	// The target has been through two reopens.
	for _, e := range []string{"E1", "E2"} {
		if _, err := (planner.Epochs{Store: planner.SQLAdvances{DB: db}}).
			OnEvent(t.Context(), "/spec", planner.CauseTicketReopen, e); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}

	f := completionDetector(db)
	f.detect.Issues = stubActive{}
	got, err := f.Detection(t.Context(), "/spec")
	if err != nil {
		t.Fatalf("detection: %v", err)
	}
	if !got.Armed {
		t.Fatalf("detection did not arm: %s", got.Reason)
	}
	if got.Epoch != 2 {
		t.Errorf("the detection carries epoch %d, not the target's own", got.Epoch)
	}
}
