package main

import (
	"bytes"
	"strings"
	"testing"

	"kriya/internal/planner"
)

func TestThePlanCommandRestoresAParkedHead(t *testing.T) {
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	plans := planner.SQLPlans{DB: db}
	if err := plans.Upsert(t.Context(), planner.Plan{
		Key: "plan-parked", TargetKey: "/spec", SpecHash: "h", Generation: 1,
		State: planner.PlanAwaitingOperator, Error: "the tracker refused the epic",
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO plan_head (target_key, current, generation, fence) VALUES (?, ?, 1, 1)`,
		"/spec", "plan-parked"); err != nil {
		t.Fatalf("seed head: %v", err)
	}

	var out bytes.Buffer
	if err := planCommand(t.Context(), &out, db, []string{"restore", "plan-parked"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(out.String(), planner.PlanPending) {
		t.Errorf("the operator was not told the new state:\n%s", out.String())
	}

	got, found, err := plans.ByKey(t.Context(), "plan-parked")
	if err != nil || !found {
		t.Fatalf("read plan: %v found=%v", err, found)
	}
	if got.State != planner.PlanPending {
		t.Errorf("the plan is %q after a restore", got.State)
	}
}

func TestThePlanCommandReportsWhatItBecameNotWhatWasAsked(t *testing.T) {
	// A retry that finds the head has outrun the candidate reports terminal
	// historical. Printing a cheerful "retried" would leave the operator
	// waiting for a build that can never start.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	plans := planner.SQLPlans{DB: db}
	for _, p := range []planner.Plan{
		{Key: "plan-head", TargetKey: "/spec", SpecHash: "h", Generation: 9, State: planner.PlanActive},
		{Key: "plan-outrun", TargetKey: "/spec", SpecHash: "h", Generation: 3,
			State: planner.PlanAwaitingOperator, Error: "lost the CAS"},
	} {
		if err := plans.Upsert(t.Context(), p); err != nil {
			t.Fatalf("seed %s: %v", p.Key, err)
		}
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO plan_head (target_key, current, generation, fence) VALUES (?, ?, 2, 0)`,
		"/spec", "plan-head"); err != nil {
		t.Fatalf("seed head: %v", err)
	}

	var out bytes.Buffer
	if err := planCommand(t.Context(), &out, db, []string{"retry", "plan-outrun"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	printed := out.String()
	if !strings.Contains(printed, planner.PlanHistorical) {
		t.Errorf("the operator was not told the retry is terminal:\n%s", printed)
	}
	if !strings.Contains(printed, "fresh intake") {
		t.Errorf("the operator was not told what to do instead:\n%s", printed)
	}
}

func TestThePlanCommandRefusesAnUnknownAction(t *testing.T) {
	db := openTemp(t)
	var out bytes.Buffer
	for _, args := range [][]string{{}, {"restore"}, {"delete", "plan-1"}} {
		if err := planCommand(t.Context(), &out, db, args); err == nil {
			t.Errorf("planCommand accepted %v", args)
		}
	}
}
