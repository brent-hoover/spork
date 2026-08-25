package planner_test

import (
	"context"
	"testing"

	"kriya/internal/planner"
)

func TestAnUnresolvedReopenObligationOutlivesItsClaim(t *testing.T) {
	// The obligation is about the EPIC, not about one attempt: it may be
	// closed over work that returned. A later attempt knowing nothing about
	// that does not make it untrue, and clearing the flag would lose the only
	// record that a compensating reopen is due.
	s := planner.SQLClaims{DB: sqlDB(t,
		planner.ClaimMigration, planner.CloseMigration,
		planner.ReopenOwedMigration, planner.WatermarkMigration)}

	if err := s.Upsert(context.Background(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionStale, Epoch: 0,
		ReopenOwed: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The next attempt, which knows nothing about the obligation.
	if err := s.Upsert(context.Background(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionSubmitting, Epoch: 1,
		SubmissionKey: "sub-1",
	}); err != nil {
		t.Fatalf("next attempt: %v", err)
	}
	got, found, err := s.Find(context.Background(), "/spec")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if !got.ReopenOwed {
		t.Error("a later attempt erased an unresolved reopen obligation")
	}
	// And the rest of the row IS the new attempt's.
	if got.Epoch != 1 || got.SubmissionKey != "sub-1" {
		t.Errorf("the new attempt did not take: %+v", got)
	}
}

func TestAClaimWithNoObligationStaysThatWay(t *testing.T) {
	// The control: the flag is sticky, not permanently on. A build that never
	// left an epic wrongly closed must not grow an obligation from nowhere.
	s := planner.SQLClaims{DB: sqlDB(t,
		planner.ClaimMigration, planner.CloseMigration,
		planner.ReopenOwedMigration, planner.WatermarkMigration)}
	for _, epoch := range []int{0, 1, 2} {
		if err := s.Upsert(context.Background(), planner.CompletionClaim{
			TargetKey: "/spec", State: planner.CompletionSubmitting, Epoch: epoch,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	got, _, _ := s.Find(context.Background(), "/spec")
	if got.ReopenOwed {
		t.Error("an obligation appeared from nowhere")
	}
}

func TestTheWatermarkSurvivesTheRoundTrip(t *testing.T) {
	s := planner.SQLClaims{DB: sqlDB(t,
		planner.ClaimMigration, planner.CloseMigration,
		planner.ReopenOwedMigration, planner.WatermarkMigration)}
	if err := s.Upsert(context.Background(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionSubmitting, Watermark: "42",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _, _ := s.Find(context.Background(), "/spec")
	if got.Watermark != "42" {
		t.Errorf("the watermark came back %q", got.Watermark)
	}
}
