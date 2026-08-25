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
		Key: "plan-1", TargetKey: "/spec", SpecHash: "hash1",
		State: planner.PlanActive, Completed: true, Tickets: 2,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	for _, tk := range []planner.Ticket{
		{Title: "Create a short link", IssueID: "issue-7", Criteria: []string{"AC-valid-url"},
			Plan: "plan-1", Ordinal: 0},
		{Title: "Redirect", IssueID: "issue-8", Criteria: []string{"AC-redirect"},
			Plan: "plan-1", Ordinal: 1},
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
		Key: "plan-1", TargetKey: "/spec", SpecHash: "hash1",
		State: planner.PlanActive, Completed: true,
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

func TestTheReportCoversOnlyTheCurrentPlan(t *testing.T) {
	// A target accumulates plans as decompositions supersede each other. A
	// target-scoped read listed every historical generation's tickets under
	// the CURRENT plan's spec hash — a report claiming one snapshot produced
	// work that three of them did.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	plans := planner.SQLPlans{DB: db}
	if err := plans.Upsert(t.Context(), planner.Plan{
		Key: "plan-old", TargetKey: "/spec", SpecHash: "hash-old",
		State: planner.PlanSuperseded, Completed: true, Tickets: 1,
	}); err != nil {
		t.Fatalf("seed the superseded plan: %v", err)
	}
	if err := plans.Upsert(t.Context(), planner.Plan{
		Key: "plan-new", TargetKey: "/spec", SpecHash: "hash-new",
		State: planner.PlanActive, Completed: true, Tickets: 1,
	}); err != nil {
		t.Fatalf("seed the head plan: %v", err)
	}
	tickets := planner.SQLTickets{DB: db}
	for _, tk := range []planner.Ticket{
		{Title: "Abandoned approach", IssueID: "issue-1", Plan: "plan-old", Ordinal: 0},
		{Title: "Create a short link", IssueID: "issue-2", Plan: "plan-new", Ordinal: 0},
	} {
		if err := tickets.Put(t.Context(), "/spec", tk); err != nil {
			t.Fatalf("seed ticket: %v", err)
		}
	}

	got, err := reportFor(db).Render(t.Context(), "/spec")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got, "Create a short link") {
		t.Errorf("the report omits the head plan's work:\n%s", got)
	}
	if strings.Contains(got, "Abandoned approach") {
		t.Errorf("the report includes a superseded plan's work:\n%s", got)
	}
	if !strings.Contains(got, "Tickets: 1") {
		t.Errorf("the report does not count one ticket:\n%s", got)
	}
}
