package planner_test

import (
	"context"
	"testing"

	"kriya/internal/planner"
)

func TestACompletionClaimSurvivesTheRoundTrip(t *testing.T) {
	// Recovery rebuilds the original request from these fields, so every one
	// of them has to come back.
	s := planner.SQLClaims{DB: sqlDB(t, planner.ClaimMigration, planner.CloseMigration, planner.ReopenOwedMigration,
		planner.WatermarkMigration)}
	want := planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionSubmitting, Epoch: 3,
		SubmissionKey: "sub-key", ReportKey: "doc-key", PendingReport: "# Done",
		ReportDoc: "doc-1", ReportVersion: "ver-1",
		ReviewID: "review-1", ReviewRevision: 2, SubtreeRevision: 7,
	}
	if err := s.Upsert(context.Background(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.Find(context.Background(), "/spec")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got != want {
		t.Errorf("read back %+v", got)
	}
}

func TestSubmittingClaimsComeBackWhole(t *testing.T) {
	s := planner.SQLClaims{DB: sqlDB(t, planner.ClaimMigration, planner.CloseMigration, planner.ReopenOwedMigration,
		planner.WatermarkMigration)}
	for _, c := range []planner.CompletionClaim{
		{TargetKey: "/b", State: planner.CompletionSubmitting, SubmissionKey: "k-b", Epoch: 1},
		{TargetKey: "/a", State: planner.CompletionSubmitting, SubmissionKey: "k-a", Epoch: 2},
		{TargetKey: "/c", State: planner.CompletionSubmitted, SubmissionKey: "k-c"},
	} {
		if err := s.Upsert(context.Background(), c); err != nil {
			t.Fatalf("upsert %s: %v", c.TargetKey, err)
		}
	}
	got, err := s.Submitting(context.Background())
	if err != nil {
		t.Fatalf("submitting: %v", err)
	}
	if len(got) != 2 || got[0].TargetKey != "/a" || got[1].TargetKey != "/b" {
		t.Fatalf("listed %+v", got)
	}
	if got[0].SubmissionKey != "k-a" || got[0].Epoch != 2 {
		t.Errorf("the claim came back %+v", got[0])
	}
}

func TestAClaimStoreWithNoTableIsNotEmpty(t *testing.T) {
	s := planner.SQLClaims{DB: sqlDB(t)}
	if err := s.Upsert(context.Background(), planner.CompletionClaim{TargetKey: "/a"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
	if _, _, err := s.Find(context.Background(), "/a"); err == nil {
		t.Error("a missing table read as an absent claim")
	}
	if _, err := s.Submitting(context.Background()); err == nil {
		t.Error("a missing table read as no claims in flight")
	}
}
