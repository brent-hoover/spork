package orchestrator_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"kriya/internal/orchestrator"
	"kriya/internal/planner"
)

// runStore opens a migrated run store.
func runStore(t *testing.T) orchestrator.SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, schema := range []string{
		orchestrator.Migration, orchestrator.RoundLimitMigration,
		orchestrator.SubmissionMigration, orchestrator.CompletionMigration,
		orchestrator.PopMigration, orchestrator.CursorMigration,
		orchestrator.MergeMigration, orchestrator.ResourceMigration,
		orchestrator.HeadMigration, orchestrator.IssueMigration,
		orchestrator.StallMigration, orchestrator.StartedMigration,
		orchestrator.FindingMigration, orchestrator.PopKeyMigration,
	} {
		for _, stmt := range strings.Split(schema, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migrate %q: %v", stmt, err)
			}
		}
	}
	return orchestrator.SQLStore{DB: db}
}

// nilGuard admits everything, so a store test can exercise the reservation
// without a fence.
type nilGuard struct{ refuse error }

func (g nilGuard) AdmitWithin(context.Context, planner.Tx, int) error { return g.refuse }

func TestAReservationWritesAQueuedRunWithItsPopKey(t *testing.T) {
	// The shape is the signal: queued, with a pop key and no issue, is
	// precisely "a claim may have been made and we do not know what it got".
	s := runStore(t)
	ctx := context.Background()
	if err := s.Reserve(ctx, orchestrator.BuildRun{
		ID: "run-1", Plan: "/spec", PopKey: "pop-key-1", RoundLimit: 4,
	}, nilGuard{}, 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	unbound, err := s.Unbound(ctx)
	if err != nil {
		t.Fatalf("unbound: %v", err)
	}
	if len(unbound) != 1 {
		t.Fatalf("the reservation left %d unbound runs", len(unbound))
	}
	if unbound[0].PopKey != "pop-key-1" {
		t.Errorf("the reservation recorded pop key %q", unbound[0].PopKey)
	}
	if unbound[0].State != orchestrator.StateQueued {
		t.Errorf("the reservation is %q", unbound[0].State)
	}
	if unbound[0].RoundLimit != 4 {
		t.Errorf("the reservation lost its round limit: %d", unbound[0].RoundLimit)
	}
}

func TestARefusedAdmissionWritesNoRun(t *testing.T) {
	// The run and the admission are ONE transaction. A refused admission that
	// still left a run behind would leave recovery a reservation for a claim
	// that never happened.
	s := runStore(t)
	ctx := context.Background()
	err := s.Reserve(ctx, orchestrator.BuildRun{ID: "run-1", Plan: "/spec", PopKey: "k"},
		nilGuard{refuse: &planner.ErrFenced{UnactivatedHeads: 1}}, 0)
	if err == nil {
		t.Fatal("a refused admission reserved anyway")
	}
	unbound, err := s.Unbound(ctx)
	if err != nil {
		t.Fatalf("unbound: %v", err)
	}
	if len(unbound) != 0 {
		t.Errorf("a refused admission left %d runs behind", len(unbound))
	}
}

func TestBindingIsConditionalOnStillBeingUnbound(t *testing.T) {
	// A replayed bind against a run that has moved on would drag it back to
	// the ticket it started with, discarding whatever progress it made.
	s := runStore(t)
	ctx := context.Background()
	if err := s.Reserve(ctx, orchestrator.BuildRun{
		ID: "run-1", Plan: "/spec", PopKey: "k",
	}, nilGuard{}, 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := s.Bind(ctx, "run-1", "issue-1", "T-1"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// The run moves on.
	got, _, err := s.Find(ctx, "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	got.State = orchestrator.StateDevLoop
	got.Head = "abc123"
	if err := s.Upsert(ctx, got); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// A replayed bind must change nothing.
	if err := s.Bind(ctx, "run-1", "issue-1", "T-1"); err != nil {
		t.Fatalf("replayed bind: %v", err)
	}
	after, _, err := s.Find(ctx, "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if after.State != orchestrator.StateDevLoop || after.Head != "abc123" {
		t.Errorf("a replayed bind dragged the run back: %+v", after)
	}
}

func TestSettlingEmptyIsConditionalToo(t *testing.T) {
	s := runStore(t)
	ctx := context.Background()
	if err := s.Reserve(ctx, orchestrator.BuildRun{
		ID: "run-1", Plan: "/spec", PopKey: "k",
	}, nilGuard{}, 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := s.Bind(ctx, "run-1", "issue-1", "T-1"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// A bound run must never be settled as no-work: it owns a real claim.
	if err := s.SettleEmpty(ctx, "run-1"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, _, err := s.Find(ctx, "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.State == orchestrator.StateNoWork {
		t.Error("a run that owns a claim was settled as no-work")
	}
}

func TestAReplayedReservationLandsOnTheSameRun(t *testing.T) {
	// The run id is derived from the pop key, so a replayed reservation must
	// not create a second run for one claim.
	s := runStore(t)
	ctx := context.Background()
	for range 2 {
		if err := s.Reserve(ctx, orchestrator.BuildRun{
			ID: "run-1", Plan: "/spec", PopKey: "k",
		}, nilGuard{}, 0); err != nil {
			t.Fatalf("reserve: %v", err)
		}
	}
	runs, err := s.ForPlan(ctx, "/spec")
	if err != nil {
		t.Fatalf("for plan: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("two reservations produced %d runs", len(runs))
	}
}
