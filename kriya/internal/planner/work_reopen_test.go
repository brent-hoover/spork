package planner_test

import (
	"context"
	"testing"
)

func TestAReopenAndRecompleteBeforeConsumptionStillAdvances(t *testing.T) {
	// The hole in reading the LIVE status: a ticket that reopened and
	// recompleted before kriya consumed either event reads as complete, the
	// cursor moves past both, and the stale completion claim survives —
	// exactly the case the spec says done must be withheld for.
	//
	// The event itself is the evidence. Every planned ticket was complete
	// when the claim was captured, so a status change on one AFTER that is a
	// reopen whatever the status reads now.
	w := watcher(t, reopenOf("issue-1"), map[string]string{"issue-1": "complete"}).
		stamped(t).plans(t, "issue-1")

	n, _, err := w.work.Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if n != 1 {
		t.Fatalf("a reopen-and-recomplete advanced %d times", n)
	}
	if w.epoch(t) != 1 {
		t.Errorf("the epoch is %d", w.epoch(t))
	}
	if _, ok := w.advances.stamped["/spec"]; ok {
		t.Error("the stale completion survived")
	}
}
