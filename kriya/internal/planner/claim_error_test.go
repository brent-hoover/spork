package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

func TestAnUnreadableEpochStopsASubmission(t *testing.T) {
	// The epoch scopes the submission key. Submitting without knowing it
	// would present a key derived from zero and collide with a first attempt
	// that may be long spent.
	advances := newMemAdvances()
	advances.err = errors.New("disk full")
	c := claimer(newMemClaims(), newDocCatalog(), newReviewDesk(),
		&staticReport{body: "# Done"}, advances)
	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 7); err == nil {
		t.Fatal("a submission proceeded without knowing its epoch")
	}
}

func TestAnUnrenderableReportStopsASubmission(t *testing.T) {
	report := &staticReport{err: errors.New("no plan to report on")}
	c := claimer(newMemClaims(), newDocCatalog(), newReviewDesk(), report, newMemAdvances())
	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 7); err == nil {
		t.Fatal("a submission proceeded with no report")
	}
}

func TestAnUnlistableClaimStoreStopsRecovery(t *testing.T) {
	// A recovery that could not read what is in flight would report zero
	// claims recovered and leave every one of them stranded.
	claims := newMemClaims()
	claims.err = errors.New("disk full")
	c := claimer(claims, newDocCatalog(), newReviewDesk(),
		&staticReport{body: "# Done"}, newMemAdvances())
	if _, err := c.Recover(context.Background(),
		func(planner.CompletionClaim) (string, string, error) {
			return "p-1", "epic-1", nil
		}); err == nil {
		t.Fatal("an unreadable claim store read as nothing in flight")
	}
}

func TestARecoveryThatCannotResolveItsTargetStops(t *testing.T) {
	// The project and epic come from the target's row. A claim whose target
	// is gone cannot be replayed against anything, and guessing would submit
	// into the wrong project.
	claims, docs, reviews := newMemClaims(), newDocCatalog(), newReviewDesk()
	report := &staticReport{body: "# Done"}
	docs.err = errors.New("crash")
	if _, err := claimer(claims, docs, reviews, report, newMemAdvances()).
		Submit(context.Background(), "/spec", "p-1", "epic-1", 7); err == nil {
		t.Fatal("expected the crash")
	}
	docs.err = nil
	if _, err := claimer(claims, docs, reviews, report, newMemAdvances()).
		Recover(context.Background(), func(planner.CompletionClaim) (string, string, error) {
			return "", "", errors.New("no build target for this claim")
		}); err == nil {
		t.Fatal("a claim with no target was replayed anyway")
	}
}

func TestNothingInFlightRecoversNothing(t *testing.T) {
	n, err := claimer(newMemClaims(), newDocCatalog(), newReviewDesk(),
		&staticReport{body: "# Done"}, newMemAdvances()).
		Recover(context.Background(), func(planner.CompletionClaim) (string, string, error) {
			return "p-1", "epic-1", nil
		})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 0 {
		t.Errorf("recovered %d claims from an empty store", n)
	}
}

func TestAnUnwritableClaimStopsTheReviewRecord(t *testing.T) {
	// The review landed but its id was never recorded. Reporting success
	// would leave a review a human can approve that kriya cannot find.
	claims := newMemClaims()
	docs, reviews := newDocCatalog(), newReviewDesk()
	c := claimer(claims, docs, reviews, &staticReport{body: "# Done"}, newMemAdvances())
	// The first two writes succeed; the third — recording the review — fails.
	claims.failAfter = 2
	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 7); err == nil {
		t.Fatal("a review whose id was never recorded read as submitted")
	}
}

func TestADetectorReasonNamesEveryBlocker(t *testing.T) {
	// The operator gets the whole list, not the first one found: fixing one
	// and coming back for the next is a slow way to learn there were three.
	plans, tickets := newMemPlans(), newMemTickets()
	live := &liveIssues{rows: []planner.LiveIssue{
		{ID: "issue-a", Status: "open"},
		{ID: "issue-b", Status: "blocked"},
	}}
	plannedTarget(t, plans, tickets, planner.PlanCompleted, "issue-a")

	got, err := detector(plans, tickets, live).Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(got.Blocking) != 2 {
		t.Errorf("reported %d blockers: %v", len(got.Blocking), got.Blocking)
	}
	for _, want := range []string{"issue-a", "issue-b"} {
		if !contains(got.Blocking, want) {
			t.Errorf("%s is missing from %v", want, got.Blocking)
		}
	}
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}
