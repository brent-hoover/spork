package planner_test

import (
	"context"
	"strings"
	"testing"

	"kriya/internal/planner"
)

func TestTheEpicItselfNeverBlocksItsOwnCompletion(t *testing.T) {
	// The umbrella epic is OPEN for exactly as long as the build runs — that
	// is what closing it means. Counting it as outstanding work makes
	// completion unable to arm, ever: the thing being completed blocks its
	// own completion.
	plans, tickets := newMemPlans(), newMemTickets()
	live := &liveIssues{rows: []planner.LiveIssue{{ID: "epic-1", Status: "open"}}}
	plannedTarget(t, plans, tickets, planner.PlanCompleted, "issue-1")

	d := detector(plans, tickets, live)
	d.Epic = "epic-1"
	got, err := d.Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !got.Armed {
		t.Errorf("the epic blocked its own completion: %s", got.Reason)
	}
}

func TestAnotherEpicStillBlocks(t *testing.T) {
	// The control: only THIS target's epic is excused. Another open epic in
	// the project is work, and completing over it would close an umbrella on
	// a subtree nobody finished.
	plans, tickets := newMemPlans(), newMemTickets()
	live := &liveIssues{rows: []planner.LiveIssue{{ID: "epic-other", Status: "open"}}}
	plannedTarget(t, plans, tickets, planner.PlanCompleted, "issue-1")

	d := detector(plans, tickets, live)
	d.Epic = "epic-1"
	got, err := d.Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.Armed {
		t.Error("another target's open epic did not block")
	}
	if !strings.Contains(got.Reason, "epic-other") {
		t.Errorf("the reason is %q", got.Reason)
	}
}

func TestWithNoEpicNamedEverythingActiveStillBlocks(t *testing.T) {
	// A detector told no epic excuses nothing. Guessing which issue is the
	// umbrella would be a way to excuse real work by accident.
	plans, tickets := newMemPlans(), newMemTickets()
	live := &liveIssues{rows: []planner.LiveIssue{{ID: "epic-1", Status: "open"}}}
	plannedTarget(t, plans, tickets, planner.PlanCompleted, "issue-1")

	got, err := detector(plans, tickets, live).Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.Armed {
		t.Error("a detector with no epic named excused one anyway")
	}
}
