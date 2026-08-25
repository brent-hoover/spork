package planner_test

import (
	"context"
	"testing"

	"kriya/internal/planner"
)

func TestAnEqualGenerationCandidateIsNotNewEnough(t *testing.T) {
	// STRICTLY newer. An equal-generation candidate is a competitor from the
	// same intake — a different spec hash under one generation — and letting
	// it through would make two plans of the same age able to supersede each
	// other in either order, so which one ends up head depends on arrival.
	//
	// The older-loses case does not test this: 1 >= 2 is false either way,
	// so a non-strict comparison passes every scenario built around it.
	if planner.Eligible(2, 2) {
		t.Error("a candidate no newer than the head was allowed to replace it")
	}
	if !planner.Eligible(3, 2) {
		t.Error("a strictly newer candidate was refused")
	}
	if planner.Eligible(1, 2) {
		t.Error("a stale candidate was allowed to replace a newer head")
	}
}

func TestAnEqualGenerationLoserIsTerminalNotParked(t *testing.T) {
	// It can never win a retry — it is not strictly newer and never will be —
	// so parking it would put something in the operator's inbox that the CAS
	// can only reject again.
	if got := planner.LandingFor(2, 2); got != planner.PlanHistorical {
		t.Errorf("an equal-generation loser landed in %q, want %q", got, planner.PlanHistorical)
	}
}

func TestAStillEligibleLoserParksForTheOperator(t *testing.T) {
	// It lost a race it could win on retry — the head it lost to may itself
	// be superseded — so the operator gets something actionable.
	if got := planner.LandingFor(5, 2); got != planner.PlanAwaitingOperator {
		t.Errorf("a still-eligible loser landed in %q, want %q", got, planner.PlanAwaitingOperator)
	}
}

// TestALosingCandidateLandsInsideTheVerdictTransaction covers what used to be
// a second write after the CAS committed.
//
// Landed afterwards, another replacement or a crash in between could leave a
// candidate pending — indistinguishable from a plan still being built, so
// recovery would keep resuming one that can never win — or parked
// awaiting-operator after it had already become permanently ineligible.
func TestALosingCandidateLandsInsideTheVerdictTransaction(t *testing.T) {
	heads, plans := headStore(t)
	ctx := context.Background()
	install(t, heads, plans, candidate(2, planner.PlanPending))

	stale := candidate(1, planner.PlanPending)
	if err := plans.Upsert(ctx, stale); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	verdict, err := heads.Replace(ctx, stale)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if verdict.Won {
		t.Fatal("a stale candidate took the head")
	}

	// The row says so already, with no second call from the caller.
	got, found, err := plans.ByKey(ctx, stale.Key)
	if err != nil || !found {
		t.Fatalf("read candidate: %v found=%v", err, found)
	}
	if got.State != planner.PlanHistorical {
		t.Errorf("the loser is %q after the CAS committed", got.State)
	}
	if got.Error == "" {
		t.Error("the loser records no cause")
	}
}

func TestAWinningCandidateIsNotLandedByTheCAS(t *testing.T) {
	// The winner's state is written by decomposition's phases. Landing it in
	// the CAS would overwrite the phase progress a crash resumes from.
	heads, plans := headStore(t)
	ctx := context.Background()
	first := candidate(1, planner.PlanPending)
	install(t, heads, plans, first)
	if err := heads.Activate(ctx, first.Key); err != nil {
		t.Fatalf("activate: %v", err)
	}

	second := candidate(2, planner.PlanPending)
	if err := plans.Upsert(ctx, second); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if _, err := heads.Replace(ctx, second); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _, err := plans.ByKey(ctx, second.Key)
	if err != nil {
		t.Fatalf("read winner: %v", err)
	}
	if got.State != planner.PlanPending {
		t.Errorf("the CAS moved the winner to %q", got.State)
	}
}
