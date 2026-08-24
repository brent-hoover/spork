package planner_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kriya/internal/planner"
)

// liveIssues answers what the tracker currently holds for a project.
type liveIssues struct {
	rows      []planner.LiveIssue
	watermark string
	err       error
	asked     int
}

func (l *liveIssues) Active(_ context.Context, _ string) ([]planner.LiveIssue, string, error) {
	l.asked++
	if l.err != nil {
		return nil, "", l.err
	}
	return l.rows, l.watermark, nil
}

func detector(plans *memPlans, tickets *memTickets, live *liveIssues) planner.Detector {
	return planner.Detector{Plans: plans, Tickets: tickets, Issues: live}
}

// memTickets is an in-memory TicketStore that can list a target's set.
type memTickets struct {
	rows map[string]planner.Ticket
	err  error
}

func newMemTickets() *memTickets { return &memTickets{rows: map[string]planner.Ticket{}} }

func (m *memTickets) Put(_ context.Context, _ string, t planner.Ticket) error {
	m.rows[t.IssueID] = t
	return nil
}

func (m *memTickets) Find(_ context.Context, _, issue string) (planner.Ticket, bool, error) {
	t, ok := m.rows[issue]
	return t, ok, nil
}

func (m *memTickets) ForTarget(_ context.Context, _ string) ([]planner.Ticket, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []planner.Ticket
	for _, t := range m.rows {
		out = append(out, t)
	}
	return out, nil
}

func plannedTarget(t *testing.T, plans *memPlans, tickets *memTickets, state string, issues ...string) {
	t.Helper()
	if err := plans.Upsert(context.Background(), planner.Plan{
		TargetKey: "/spec", SpecHash: "hash1", State: state, Tickets: len(issues),
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	for _, id := range issues {
		if err := tickets.Put(context.Background(), "/spec",
			planner.Ticket{Title: id, IssueID: id}); err != nil {
			t.Fatalf("seed ticket: %v", err)
		}
	}
}

func TestAPlanStillDecomposingNeverArms(t *testing.T) {
	// "completion never fires against a partial plan". Every ticket created
	// so far being complete says nothing about a build whose ticket set is
	// still growing.
	plans, tickets, live := newMemPlans(), newMemTickets(), &liveIssues{}
	plannedTarget(t, plans, tickets, planner.PlanDecomposing, "issue-1")

	got, err := detector(plans, tickets, live).Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.Armed {
		t.Error("detection armed against a plan that is still decomposing")
	}
	if !strings.Contains(got.Reason, "decomposing") {
		t.Errorf("the reason is %q, want it to name the incomplete plan", got.Reason)
	}
}

func TestACompletedPlanWithNoActiveWorkArms(t *testing.T) {
	plans, tickets, live := newMemPlans(), newMemTickets(), &liveIssues{watermark: "w-7"}
	plannedTarget(t, plans, tickets, planner.PlanCompleted, "issue-1", "issue-2")

	got, err := detector(plans, tickets, live).Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !got.Armed {
		t.Fatalf("a whole plan with nothing active did not arm: %s", got.Reason)
	}
	// The watermark travels with the answer: the fence a claim is captured
	// against has to be the one this read saw.
	if got.Watermark != "w-7" {
		t.Errorf("the detection carries watermark %q", got.Watermark)
	}
}

func TestAPlanTicketStillActiveBlocksArming(t *testing.T) {
	plans, tickets := newMemPlans(), newMemTickets()
	live := &liveIssues{rows: []planner.LiveIssue{
		{ID: "issue-2", Status: "in-progress"},
	}}
	plannedTarget(t, plans, tickets, planner.PlanCompleted, "issue-1", "issue-2")

	got, err := detector(plans, tickets, live).Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.Armed {
		t.Error("detection armed with a plan ticket still in progress")
	}
	if !strings.Contains(got.Reason, "issue-2") {
		t.Errorf("the reason is %q, want it to name the ticket", got.Reason)
	}
}

func TestAnOpenUnplannedTicketBlocksArming(t *testing.T) {
	// A ticket the plan never produced, parented under the epic at bind time.
	// sutra's close gate would refuse the epic anyway — it is a descendant —
	// so arming would submit a completion review that can never close.
	plans, tickets := newMemPlans(), newMemTickets()
	live := &liveIssues{rows: []planner.LiveIssue{{ID: "issue-99", Status: "open"}}}
	plannedTarget(t, plans, tickets, planner.PlanCompleted, "issue-1")

	got, err := detector(plans, tickets, live).Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.Armed {
		t.Error("detection armed with an unplanned ticket still open")
	}
	if !strings.Contains(got.Reason, "issue-99") {
		t.Errorf("the reason is %q, want it to name the unplanned ticket", got.Reason)
	}
}

func TestABlockedUnplannedTicketBlocksArming(t *testing.T) {
	// Blocked is ACTIVE work — resolved through the live queue — not work
	// that has gone away.
	plans, tickets := newMemPlans(), newMemTickets()
	live := &liveIssues{rows: []planner.LiveIssue{{ID: "issue-99", Status: "blocked"}}}
	plannedTarget(t, plans, tickets, planner.PlanCompleted, "issue-1")

	got, err := detector(plans, tickets, live).Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.Armed {
		t.Error("detection armed with a blocked ticket outstanding")
	}
	if !strings.Contains(got.Reason, "blocked") {
		t.Errorf("the reason is %q, want it to name the status", got.Reason)
	}
}

func TestATargetWithNoPlanNeverArms(t *testing.T) {
	// Nothing has been planned for it. Arming would report a build done that
	// was never started.
	got, err := detector(newMemPlans(), newMemTickets(), &liveIssues{}).
		Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.Armed {
		t.Error("a target with no plan at all armed")
	}
}

func TestAnUnreadableTrackerNeverArms(t *testing.T) {
	// "I could not read the queue" is not "the queue is empty". Arming on an
	// unreadable tracker would submit a completion review over work nobody
	// looked at.
	plans, tickets := newMemPlans(), newMemTickets()
	live := &liveIssues{err: errors.New("sutra unavailable")}
	plannedTarget(t, plans, tickets, planner.PlanCompleted, "issue-1")

	if _, err := detector(plans, tickets, live).
		Detect(context.Background(), "/spec", "project-1"); err == nil {
		t.Fatal("an unreachable tracker read as an empty queue")
	}
}

func TestAnUnreadablePlanNeverArms(t *testing.T) {
	plans := newMemPlans()
	plans.err = errors.New("disk full")
	if _, err := detector(plans, newMemTickets(), &liveIssues{}).
		Detect(context.Background(), "/spec", "project-1"); err == nil {
		t.Fatal("an unreadable plan read as an absent one")
	}
}

func TestACompletePlanTicketDoesNotBlockItself(t *testing.T) {
	// The control for the active-ticket tests: a plan ticket that the tracker
	// reports as complete is not active work, and must not block forever.
	plans, tickets := newMemPlans(), newMemTickets()
	live := &liveIssues{rows: []planner.LiveIssue{}}
	plannedTarget(t, plans, tickets, planner.PlanCompleted, "issue-1", "issue-2")

	got, err := detector(plans, tickets, live).Detect(context.Background(), "/spec", "project-1")
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !got.Armed {
		t.Errorf("a plan whose tickets are all complete did not arm: %s", got.Reason)
	}
}
