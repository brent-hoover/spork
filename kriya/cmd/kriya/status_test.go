package main

import (
	"strings"
	"testing"
	"time"

	"kriya/internal/clock"
	"kriya/internal/gates"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
)

func TestTheStatusReportGathersEveryStore(t *testing.T) {
	// The operator's picture comes from five stores. A gatherer that missed
	// one would show a build that looks fine while the part it skipped is
	// what is wrong.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLTargets{DB: db}).Upsert(t.Context(), planner.BuildTarget{
		TargetKey: "/linkshort", ProjectID: "p-1", EpicID: "epic-1",
		EpicState: planner.EpicCreated,
	}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := (planner.SQLPlans{DB: db}).Upsert(t.Context(), planner.Plan{
		TargetKey: "/linkshort", SpecHash: "h", State: planner.PlanActive, Completed: true, Tickets: 2,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if err := (orchestrator.SQLStore{DB: db}).Upsert(t.Context(), orchestrator.BuildRun{
		ID: "run-1", Ticket: "MOD-api", Plan: "/linkshort", State: orchestrator.StateGates,
		Head: "C2", Attempt: 1, Started: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := (gates.SQLStore{DB: db}).Upsert(t.Context(), gates.Result{
		Build: "run-1", Module: "MOD-api", Gate: "test", Commit: "C2",
		Attempt: 1, Passed: true,
	}); err != nil {
		t.Fatalf("seed gate: %v", err)
	}
	if _, err := (orchestrator.Stalls{
		Store: orchestrator.SQLStalls{DB: db}, Now: clock.System{},
	}).Record(t.Context(), "/linkshort", 0, "outstanding work: issue-9"); err != nil {
		t.Fatalf("seed stall: %v", err)
	}

	got, err := gatherStatus(t.Context(), db)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(got.Targets) != 1 {
		t.Fatalf("gathered %d targets", len(got.Targets))
	}
	target := got.Targets[0]
	if target.Plan != planner.PlanActive || target.Tickets != 2 {
		t.Errorf("the plan came back %q with %d tickets", target.Plan, target.Tickets)
	}
	if len(target.Runs) != 1 {
		t.Fatalf("gathered %d runs", len(target.Runs))
	}
	run := target.Runs[0]
	if run.Ticket != "MOD-api" || run.Attempt != 1 || run.Started.IsZero() {
		t.Errorf("the run came back %+v", run)
	}
	// The gate POSITION: one passed, the rest not yet run.
	if len(run.Gates) != len(gates.Chain) {
		t.Fatalf("gathered %d gates", len(run.Gates))
	}
	if !run.Gates[0].Passed || run.Gates[0].Gate != "test" {
		t.Errorf("the first gate came back %+v", run.Gates[0])
	}
	if run.Gates[1].Passed {
		t.Errorf("a gate that never ran reads as passed: %+v", run.Gates[1])
	}
	if len(got.Stalls) != 1 || !strings.Contains(got.Stalls[0].Cause, "issue-9") {
		t.Errorf("the stalls came back %+v", got.Stalls)
	}
}

func TestACompletedTargetReportsItsDeclaration(t *testing.T) {
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLTargets{DB: db}).Upsert(t.Context(), planner.BuildTarget{
		TargetKey: "/paste", ProjectID: "p-1", EpicID: "epic-2",
		EpicState: planner.EpicCreated,
	}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := (planner.SQLClaims{DB: db}).Upsert(t.Context(), planner.CompletionClaim{
		TargetKey: "/paste", State: planner.CompletionComplete, Epoch: 0,
		ReviewID: "review-9", ReportVersion: "ver-2",
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if ok, err := (planner.Epochs{Store: planner.SQLAdvances{DB: db}}).
		Stamp(t.Context(), "/paste", 0); err != nil || !ok {
		t.Fatalf("stamp: %v ok=%v", err, ok)
	}

	got, err := gatherStatus(t.Context(), db)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	c := got.Targets[0].Completion
	if !c.Complete || c.Review != "review-9" || c.ReportVersion != "ver-2" {
		t.Errorf("the declaration came back %+v", c)
	}
}

func TestAnUnclaimedTargetReportsItsEpochAnyway(t *testing.T) {
	// A build that has never claimed completion is not the same as one whose
	// claim went stale, and the epoch is what tells them apart.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLTargets{DB: db}).Upsert(t.Context(), planner.BuildTarget{
		TargetKey: "/fresh", ProjectID: "p-1", EpicID: "e", EpicState: planner.EpicCreated,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := (planner.Epochs{Store: planner.SQLAdvances{DB: db}}).
		OnEvent(t.Context(), "/fresh", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got, err := gatherStatus(t.Context(), db)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	c := got.Targets[0].Completion
	if c.Complete || c.Epoch != 1 {
		t.Errorf("the declaration came back %+v", c)
	}
}

func TestARunWithNoGatedHeadReportsNoGates(t *testing.T) {
	// Six rows all reading FAILED would say the chain ran and lost. An empty
	// list says it has not run.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLTargets{DB: db}).Upsert(t.Context(), planner.BuildTarget{
		TargetKey: "/t", ProjectID: "p", EpicID: "e", EpicState: planner.EpicCreated,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := (orchestrator.SQLStore{DB: db}).Upsert(t.Context(), orchestrator.BuildRun{
		ID: "run-1", Ticket: "T", Plan: "/t", State: orchestrator.StateQueued,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	got, err := gatherStatus(t.Context(), db)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(got.Targets[0].Runs[0].Gates) != 0 {
		t.Errorf("an ungated run reported %d gates", len(got.Targets[0].Runs[0].Gates))
	}
}

func TestTheStatusCommandRunsThroughItsRealSurface(t *testing.T) {
	// An operator has no other way in.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := status(t.Context(), db, nil); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := status(t.Context(), db, []string{"--json"}); err != nil {
		t.Fatalf("status --json: %v", err)
	}
	if err := status(t.Context(), db, []string{"--nonsense"}); err == nil {
		t.Error("an unknown flag was accepted")
	}
}
