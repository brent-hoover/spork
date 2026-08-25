package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

var errStoreDown = errors.New("the plan store is unreachable")

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

// countingHeads is a HeadStore that records what it was asked.
type countingHeads struct {
	verdict   planner.Replacement
	err       error
	activated []string
}

func (c *countingHeads) Replace(context.Context, planner.Plan) (planner.Replacement, error) {
	return c.verdict, c.err
}

func (c *countingHeads) Head(context.Context, string) (planner.PlanHead, bool, error) {
	return planner.PlanHead{}, false, nil
}

func (c *countingHeads) Activate(_ context.Context, key string) error {
	c.activated = append(c.activated, key)
	return nil
}

func TestALosingCandidateLandsDurablyRatherThanStayingPending(t *testing.T) {
	// A loser left pending is indistinguishable from a plan still being
	// built, so recovery would keep resuming a candidate that can never win.
	plans := newMemPlans()
	heads := &countingHeads{verdict: planner.Replacement{
		Won:     false,
		Head:    planner.PlanHead{Current: "winning-key"},
		Landing: planner.PlanHistorical,
	}}
	losing := planner.Plan{Key: "losing-key", TargetKey: "/spec", State: planner.PlanPending}

	verdict, err := planner.Supersede(context.Background(), heads, plans, losing)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if verdict.Won {
		t.Fatal("the verdict says the loser won")
	}
	got, found, err := plans.ByKey(context.Background(), "losing-key")
	if err != nil || !found {
		t.Fatalf("the losing candidate has no row: %v found=%v", err, found)
	}
	if got.State != planner.PlanHistorical {
		t.Errorf("the loser is still %q", got.State)
	}
	// And it says why, naming the head it lost to. A park with no cause is
	// one nobody can act on.
	if got.Error == "" {
		t.Error("the losing candidate records no cause")
	}
}

func TestAWinningCandidateIsNotRewritten(t *testing.T) {
	// The winner's own state is written by decomposition's phases, not by
	// the CAS. Landing it here would overwrite the phase progress a crash
	// needs to resume from.
	plans := newMemPlans()
	heads := &countingHeads{verdict: planner.Replacement{Won: true}}
	winner := planner.Plan{Key: "winning-key", TargetKey: "/spec", State: planner.PlanPending}

	if _, err := planner.Supersede(context.Background(), heads, plans, winner); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if _, found, _ := plans.ByKey(context.Background(), "winning-key"); found {
		t.Error("winning the CAS rewrote the candidate's plan row")
	}
}

func TestAFailedReplacementIsNotALoss(t *testing.T) {
	// A store that could not answer has not said the candidate lost, and
	// landing it historical on an outage would kill a plan that might win.
	plans := newMemPlans()
	heads := &countingHeads{err: errStoreDown}
	losing := planner.Plan{Key: "k", TargetKey: "/spec", State: planner.PlanPending}

	if _, err := planner.Supersede(context.Background(), heads, plans, losing); err == nil {
		t.Fatal("a failed replacement resolved without an error")
	}
	if _, found, _ := plans.ByKey(context.Background(), "k"); found {
		t.Error("a failed replacement landed the candidate anyway")
	}
}
