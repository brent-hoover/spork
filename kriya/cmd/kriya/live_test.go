package main

import (
	"context"
	"testing"

	"kriya/internal/orchestrator"
)

func TestLiveRunsMatchOnTheIssueNotTheTitle(t *testing.T) {
	// build_run.ticket holds the ticket's TITLE and build_run.issue holds the
	// tracker id; IssueMigration exists because a previous version confused
	// them. Querying titles with issue ids matches nothing, so every live
	// build looks unclaimed and retirement defers work in flight.
	//
	// The two values are deliberately different here — identical ones would
	// pass against either column.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	runs := orchestrator.SQLStore{DB: db}
	if err := runs.Upsert(t.Context(), orchestrator.BuildRun{
		ID: "run-1", Ticket: "Create a short link", Issue: "issue-7",
		State: orchestrator.StateDevLoop,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	claimed, err := liveRuns{db: db}.Claimed(t.Context(), []string{"issue-7"})
	if err != nil {
		t.Fatalf("claimed: %v", err)
	}
	if !claimed["issue-7"] {
		t.Error("a live build's issue was not reported as claimed")
	}
}

func TestATerminalRunHoldsNothing(t *testing.T) {
	// A ticket whose only run merged, failed or was CANCELLED is released
	// work. Treating it as bound would leave dropped work open forever,
	// holding the epic with it.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	runs := orchestrator.SQLStore{DB: db}
	for n, state := range []orchestrator.State{
		orchestrator.StateMerged, orchestrator.StateCancelled, orchestrator.StateFailed,
	} {
		if err := runs.Upsert(t.Context(), orchestrator.BuildRun{
			ID: "run-" + string(rune('a'+n)), Ticket: "t",
			Issue: "issue-" + string(rune('a'+n)), State: state,
		}); err != nil {
			t.Fatalf("seed %s: %v", state, err)
		}
	}

	claimed, err := liveRuns{db: db}.Claimed(t.Context(),
		[]string{"issue-a", "issue-b", "issue-c"})
	if err != nil {
		t.Fatalf("claimed: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("terminal runs reported %v as claimed", claimed)
	}
}

func TestNoIssuesMeansNoQuery(t *testing.T) {
	claimed, err := liveRuns{db: nil}.Claimed(context.Background(), nil)
	if err != nil {
		t.Fatalf("claimed: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("an empty request returned %v", claimed)
	}
}
