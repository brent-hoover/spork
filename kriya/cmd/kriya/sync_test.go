package main

import (
	"context"
	"testing"

	"kriya/internal/orchestrator"
	"kriya/internal/planner"
)

func TestAClaimSyncsTheWorkCursorToItsWatermark(t *testing.T) {
	// A WIRING test. Every ticket completing during a build emits a status
	// change, and those events sit in the feed until consumed. Without the
	// sync the watcher reads each of them as a reopen and destroys the claim
	// the moment it exists — no build could ever complete.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	row := planner.BuildTarget{TargetKey: "/spec", EpicID: "epic-1"}
	w := workWatcher(db, row, "/spec", "actor-1")

	if err := w.SyncTo(t.Context(), "42"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	at, err := (orchestrator.SQLCursors{DB: db}).Current(t.Context(), w.Name())
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if at != "42" {
		t.Errorf("the work cursor is at %q, not the claim's watermark", at)
	}
}

func TestTheClaimCarriesTheDetectionsWatermark(t *testing.T) {
	// It is the detection's, not a fresh read: the events accounted for are
	// exactly the ones that detection saw the effects of.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLTargets{DB: db}).Upsert(t.Context(), planner.BuildTarget{
		TargetKey: "/spec", ProjectID: "p-1", EpicID: "epic-1",
		EpicState: planner.EpicCreated,
	}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := (planner.SQLPlans{DB: db}).Upsert(t.Context(), planner.Plan{
		TargetKey: "/spec", SpecHash: "h", State: planner.PlanActive, Completed: true, Tickets: 1,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	f := completionDetector(db)
	f.detect.Issues = watermarkedActive{at: "42"}
	got, err := f.Detection(t.Context(), "/spec")
	if err != nil {
		t.Fatalf("detection: %v", err)
	}
	if got.Watermark != "42" {
		t.Errorf("the detection carries watermark %q", got.Watermark)
	}
}

// watermarkedActive answers with a known feed position.
type watermarkedActive struct{ at string }

func (a watermarkedActive) Active(
	context.Context, string,
) ([]planner.LiveIssue, string, error) {
	return nil, a.at, nil
}
