package main

import (
	"strings"
	"testing"

	"kriya/internal/planner"
)

func TestTheCompletionReportNamesTheWorkAndItsCriteria(t *testing.T) {
	// A human reads this to decide whether the build is done. "The gates
	// passed" is a precondition of reaching them at all; what they need is
	// what was actually built.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLPlans{DB: db}).Upsert(t.Context(), planner.Plan{
		TargetKey: "/spec", SpecHash: "hash1", State: planner.PlanCompleted, Tickets: 2,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	for _, tk := range []planner.Ticket{
		{Title: "Create a short link", IssueID: "issue-7", Criteria: []string{"AC-valid-url"}},
		{Title: "Redirect", IssueID: "issue-8", Criteria: []string{"AC-redirect"}},
	} {
		if err := (planner.SQLTickets{DB: db}).Put(t.Context(), "/spec", tk); err != nil {
			t.Fatalf("seed ticket: %v", err)
		}
	}

	got, err := reportFor(db).Render(t.Context(), "/spec")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"Create a short link", "issue-7", "AC-valid-url", "Redirect", "AC-redirect", "hash1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report omits %q:\n%s", want, got)
		}
	}
}

func TestAPlanWithNoTicketsHasNoReport(t *testing.T) {
	// A plan stamped whole with no tickets is a decomposition that produced
	// nothing. Reporting it as finished asks a human to approve an empty claim.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLPlans{DB: db}).Upsert(t.Context(), planner.Plan{
		TargetKey: "/spec", SpecHash: "hash1", State: planner.PlanCompleted,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := reportFor(db).Render(t.Context(), "/spec"); err == nil {
		t.Fatal("an empty plan produced a completion report")
	}
}

func TestATargetWithNoPlanHasNoReport(t *testing.T) {
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := reportFor(db).Render(t.Context(), "/never-planned"); err == nil {
		t.Fatal("a target nobody planned produced a completion report")
	}
}
