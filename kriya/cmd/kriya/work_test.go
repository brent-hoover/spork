package main

import (
	"testing"

	"kriya/internal/planner"
)

func TestTheWorkWatcherIsWiredWithEveryCollaborator(t *testing.T) {
	// A WIRING test. Nothing else advances the completion epoch: a
	// collaborator the composition root forgot leaves a stamped target
	// complete forever, with a stale approval that is never fenced.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	w := workWatcher(db, planner.BuildTarget{EpicID: "epic-1"}, "/spec", "actor-1")
	for name, wired := range map[string]bool{
		"feed":    w.Feed != nil,
		"epochs":  w.Epochs.Store != nil,
		"tickets": w.Tickets != nil,
		"claims":  w.Claims != nil,
		"cursors": w.Cursors != nil,
	} {
		if !wired {
			t.Errorf("the watcher has no %s", name)
		}
	}
	// The scope, without which it would advance for another target's work or
	// skip its own epic's reopen.
	if w.TargetKey != "/spec" || w.Epic != "epic-1" || w.Actor != "actor-1" {
		t.Errorf("the watcher is scoped to %+v", w)
	}
}

func TestTheWorkCursorIsSeparateFromTheVerdictCursor(t *testing.T) {
	// Two consumers of one feed. Sharing a cursor would have each advance
	// past the other's events, which are then never offered again.
	db := openTemp(t)
	work := workWatcher(db, planner.BuildTarget{EpicID: "e"}, "/spec", "actor-1").Name()
	verdicts := verdictRouter(db, "actor-1", "/spec").Name
	if work == verdicts {
		t.Errorf("both consumers read under cursor %q", work)
	}
}

func TestATargetWithNoRowConsumesNothing(t *testing.T) {
	// The watcher needs the target's epic to know what its own reopen is. A
	// target nobody planned has none, and reaching the tracker — unreachable
	// here — would fail loudly.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := consumeWork(t.Context(), db, "/never-planned", "actor-1"); err != nil {
		t.Errorf("a target with no row reached the tracker: %v", err)
	}
}
