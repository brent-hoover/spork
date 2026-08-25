package planner_test

import (
	"context"
	"testing"

	"kriya/internal/planner"
)

// racingIssues advances the epoch while the active-issue read is in flight,
// which is the window the ordering exists to make safe.
type racingIssues struct {
	epochs planner.Epochs
	target string
}

func (r racingIssues) Active(
	ctx context.Context, _ string,
) ([]planner.LiveIssue, string, error) {
	if _, err := r.epochs.OnEvent(ctx, r.target,
		planner.CauseTicketReopen, "raced"); err != nil {
		return nil, "", err
	}
	return nil, "w-1", nil
}

func TestAnAdvanceRacingTheQueueLeavesTheAnswerOnTheOlderEpoch(t *testing.T) {
	// The safe direction. A claim bound to the OLDER epoch fails the stamp's
	// CAS and nothing is stamped; one bound to the newer epoch would PASS the
	// CAS having checked a queue state that epoch never had.
	plans, tickets := newMemPlans(), newMemTickets()
	plannedTarget(t, plans, tickets, true, "issue-1")
	advances := newMemAdvances()
	epochs := planner.Epochs{Store: advances}

	d := planner.Detector{
		Plans: plans, Tickets: tickets, Epochs: &epochs,
		Issues: racingIssues{epochs: epochs, target: "/spec"},
	}
	got, err := d.Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !got.Armed {
		t.Fatalf("detection did not arm: %s", got.Reason)
	}
	current, _ := epochs.Current(context.Background(), "/spec")
	if current != 1 {
		t.Fatalf("the race did not advance the epoch: %d", current)
	}
	if got.Epoch != 0 {
		t.Errorf("the answer bound to epoch %d, the one the race produced", got.Epoch)
	}
	// And that is what makes it safe: the claim's stamp is refused.
	stamped, err := epochs.Stamp(context.Background(), "/spec", got.Epoch)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if stamped {
		t.Error("a claim from before the race stamped anyway")
	}
}
