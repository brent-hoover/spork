package reviewbridge_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"kriya/internal/reviewbridge"
)

func sqlRounds(t *testing.T, migrate bool) reviewbridge.SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if !migrate {
		return reviewbridge.SQLStore{DB: db}
	}
	for _, schema := range []string{reviewbridge.Migration, reviewbridge.ResponseMigration} {
		for _, stmt := range strings.Split(schema, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migrate: %v", err)
			}
		}
	}
	return reviewbridge.SQLStore{DB: db}
}

func TestARoundSurvivesTheRoundTrip(t *testing.T) {
	s := sqlRounds(t, true)
	want := reviewbridge.Round{
		ID: "run-1-1", Run: "run-1", Commit: "abc123", JobID: 91,
		Verdict: reviewbridge.VerdictFindings, Findings: "- **Severity**: Low",
		State: reviewbridge.RoundCommenting, Response: "Addressed in def456.",
	}
	if err := s.UpsertRound(context.Background(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	open, err := s.Unsettled(context.Background())
	if err != nil {
		t.Fatalf("unsettled: %v", err)
	}
	if len(open) != 1 || open[0] != want {
		t.Fatalf("read back %+v", open)
	}
}

func TestOnlyRoundsMidResponseAreUnsettled(t *testing.T) {
	s := sqlRounds(t, true)
	for _, r := range []reviewbridge.Round{
		{ID: "r-open", JobID: 1, Verdict: reviewbridge.VerdictPending, State: reviewbridge.RoundOpen},
		{ID: "r-commenting", JobID: 2, Verdict: reviewbridge.VerdictClean, State: reviewbridge.RoundCommenting},
		{ID: "r-closing", JobID: 3, Verdict: reviewbridge.VerdictClean, State: reviewbridge.RoundClosing},
		{ID: "r-closed", JobID: 4, Verdict: reviewbridge.VerdictClean, State: reviewbridge.RoundClosed},
	} {
		if err := s.UpsertRound(context.Background(), r); err != nil {
			t.Fatalf("upsert %s: %v", r.ID, err)
		}
	}
	open, err := s.Unsettled(context.Background())
	if err != nil {
		t.Fatalf("unsettled: %v", err)
	}
	if len(open) != 2 || open[0].ID != "r-closing" || open[1].ID != "r-commenting" {
		t.Fatalf("unsettled %+v", open)
	}
}

func TestReRecordingARoundReplacesIt(t *testing.T) {
	s := sqlRounds(t, true)
	r := reviewbridge.Round{ID: "run-1-1", JobID: 91, Verdict: reviewbridge.VerdictClean,
		State: reviewbridge.RoundCommenting, Response: "Clean pass."}
	if err := s.UpsertRound(context.Background(), r); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	r.State = reviewbridge.RoundClosed
	if err := s.UpsertRound(context.Background(), r); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	open, err := s.Unsettled(context.Background())
	if err != nil {
		t.Fatalf("unsettled: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("a settled round is still listed unsettled: %+v", open)
	}
}

func TestAnEnqueueAttemptSurvivesTheRoundTrip(t *testing.T) {
	s := sqlRounds(t, true)
	want := reviewbridge.EnqueueAttempt{
		Round: "run-1-1", Run: "run-1", Commit: "abc123",
		State: reviewbridge.AttemptUnresolved, Note: "connection reset",
	}
	if err := s.UpsertAttempt(context.Background(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	stuck, err := s.Unresolved(context.Background())
	if err != nil {
		t.Fatalf("unresolved: %v", err)
	}
	if len(stuck) != 1 || stuck[0] != want {
		t.Fatalf("read back %+v", stuck)
	}
}

func TestAPendingAttemptCountsAsUnresolved(t *testing.T) {
	// A row still reading "pending" when recovery runs is a crash in exactly
	// the window between the write and the call.
	s := sqlRounds(t, true)
	for _, a := range []reviewbridge.EnqueueAttempt{
		{Round: "r-pending", State: reviewbridge.AttemptPending},
		{Round: "r-resolved", State: reviewbridge.AttemptResolved, JobID: 91},
	} {
		if err := s.UpsertAttempt(context.Background(), a); err != nil {
			t.Fatalf("upsert %s: %v", a.Round, err)
		}
	}
	stuck, err := s.Unresolved(context.Background())
	if err != nil {
		t.Fatalf("unresolved: %v", err)
	}
	if len(stuck) != 1 || stuck[0].Round != "r-pending" {
		t.Fatalf("unresolved %+v", stuck)
	}
}

func TestAResolvedAttemptStopsBeingUnresolved(t *testing.T) {
	s := sqlRounds(t, true)
	a := reviewbridge.EnqueueAttempt{Round: "r-1", State: reviewbridge.AttemptPending}
	if err := s.UpsertAttempt(context.Background(), a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	a.State, a.JobID = reviewbridge.AttemptResolved, 91
	if err := s.UpsertAttempt(context.Background(), a); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	stuck, err := s.Unresolved(context.Background())
	if err != nil {
		t.Fatalf("unresolved: %v", err)
	}
	if len(stuck) != 0 {
		t.Errorf("a resolved attempt is still unresolved: %+v", stuck)
	}
}

func TestAPassIsKeyedByRunAndCommit(t *testing.T) {
	// A pass on an earlier commit says nothing about the branch head, which is
	// the whole point of reviewing every commit.
	s := sqlRounds(t, true)
	if err := s.UpsertRound(context.Background(), reviewbridge.Round{
		ID: "run-1-1", Run: "run-1", Commit: "abc123", JobID: 91,
		Verdict: reviewbridge.VerdictClean, State: reviewbridge.RoundClosed,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, tc := range []struct {
		run, commit string
		want        bool
	}{
		{"run-1", "abc123", true},
		{"run-1", "def456", false},
		{"run-2", "abc123", false},
	} {
		got, err := s.PassedAt(context.Background(), tc.run, tc.commit)
		if err != nil {
			t.Fatalf("passed at: %v", err)
		}
		if got != tc.want {
			t.Errorf("run %s at %s reported %v", tc.run, tc.commit, got)
		}
	}
}

func TestAFindingsVerdictIsNotAPass(t *testing.T) {
	s := sqlRounds(t, true)
	if err := s.UpsertRound(context.Background(), reviewbridge.Round{
		ID: "run-1-1", Run: "run-1", Commit: "abc123", JobID: 91,
		Verdict: reviewbridge.VerdictFindings, State: reviewbridge.RoundClosed,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.PassedAt(context.Background(), "run-1", "abc123")
	if err != nil {
		t.Fatalf("passed at: %v", err)
	}
	if got {
		t.Error("a round that reported findings satisfied the review gate")
	}
}

func TestAReviewStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	// A short list reads exactly like "no round left open".
	s := sqlRounds(t, false)
	if _, err := s.Unsettled(context.Background()); err == nil {
		t.Error("a missing table listed as no unsettled rounds")
	}
	if _, err := s.Unresolved(context.Background()); err == nil {
		t.Error("a missing table listed as no unresolved attempts")
	}
	if _, err := s.PassedAt(context.Background(), "run-1", "abc123"); err == nil {
		t.Error("a missing table read as a review that never passed")
	}
	if err := s.UpsertRound(context.Background(), reviewbridge.Round{ID: "r-1"}); err == nil {
		t.Error("a round written to a missing table reported success")
	}
	if err := s.UpsertAttempt(context.Background(), reviewbridge.EnqueueAttempt{Round: "r-1"}); err == nil {
		t.Error("an attempt written to a missing table reported success")
	}
}
