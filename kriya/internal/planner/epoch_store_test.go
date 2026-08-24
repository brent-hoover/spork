package planner_test

import (
	"context"
	"testing"

	"kriya/internal/planner"
)

func sqlAdvances(t *testing.T) planner.SQLAdvances {
	t.Helper()
	return planner.SQLAdvances{DB: sqlDB(t, planner.EpochMigration)}
}

func TestTheEpochAndItsAdvanceLogMoveTogether(t *testing.T) {
	s := sqlAdvances(t)
	e := planner.Epochs{Store: s}
	moved, err := e.OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1")
	if err != nil || !moved {
		t.Fatalf("advance: %v moved=%v", err, moved)
	}
	if got, _ := e.Current(context.Background(), "/spec"); got != 1 {
		t.Errorf("the epoch is %d", got)
	}
}

func TestAReplayedEventCollidesInSQL(t *testing.T) {
	// The primary key IS the check. Two consumers of one event both insert,
	// and exactly one wins.
	s := sqlAdvances(t)
	e := planner.Epochs{Store: s}
	for range 5 {
		if _, err := e.OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	if got, _ := e.Current(context.Background(), "/spec"); got != 1 {
		t.Errorf("five replays moved the epoch to %d", got)
	}
}

func TestAStaleClaimCannotStampInSQL(t *testing.T) {
	s := sqlAdvances(t)
	e := planner.Epochs{Store: s}
	if _, err := e.OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	claim, _ := e.Current(context.Background(), "/spec")
	if _, err := e.OnEvent(context.Background(), "/spec", planner.CauseNewWork, "E2"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	stamped, err := e.Stamp(context.Background(), "/spec", claim)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if stamped {
		t.Error("a stale claim stamped")
	}
	if is, _, _ := s.Stamped(context.Background(), "/spec"); is {
		t.Error("the target reads as complete")
	}
}

func TestAFirstEpochClaimStampsWithNoPriorRow(t *testing.T) {
	// A target whose work never came back is at epoch zero and has no state
	// row at all. Refusing to stamp it would mean no build could ever finish
	// without first being interrupted.
	s := sqlAdvances(t)
	e := planner.Epochs{Store: s}
	stamped, err := e.Stamp(context.Background(), "/spec", 0)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if !stamped {
		t.Fatal("a first-epoch claim did not stamp")
	}
	is, at, _ := s.Stamped(context.Background(), "/spec")
	if !is || at != 0 {
		t.Errorf("stamped=%v at epoch %d", is, at)
	}
}

func TestAnAdvanceClearsTheStampInSQL(t *testing.T) {
	s := sqlAdvances(t)
	e := planner.Epochs{Store: s}
	if stamped, err := e.Stamp(context.Background(), "/spec", 0); err != nil || !stamped {
		t.Fatalf("stamp: %v", err)
	}
	if _, err := e.OnEvent(context.Background(), "/spec", planner.CauseNewWork, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	is, _, err := s.Stamped(context.Background(), "/spec")
	if err != nil {
		t.Fatalf("stamped: %v", err)
	}
	if is {
		t.Error("the stamp survived work returning")
	}
}

func TestAnUnstampedTargetReadsAsIncomplete(t *testing.T) {
	if is, _, err := sqlAdvances(t).Stamped(context.Background(), "/never-seen"); err != nil || is {
		t.Errorf("a target nobody stamped reads complete=%v: %v", is, err)
	}
}

func TestAnAdvanceStoreWithNoTablesIsNotEmpty(t *testing.T) {
	s := planner.SQLAdvances{DB: sqlDB(t)}
	if _, err := s.Insert(context.Background(),
		planner.CompletionAdvance{Key: "k", TargetKey: "/spec", Cause: planner.CauseNewWork}); err == nil {
		t.Error("an insert into a missing table reported success")
	}
	if _, err := s.Epoch(context.Background(), "/spec"); err == nil {
		t.Error("a missing table read as epoch zero")
	}
	if _, err := s.Stamp(context.Background(), "/spec", 0); err == nil {
		t.Error("a stamp into a missing table reported success")
	}
	if _, _, err := s.Stamped(context.Background(), "/spec"); err == nil {
		t.Error("a missing table read as unstamped")
	}
}
