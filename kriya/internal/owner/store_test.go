package owner_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"kriya/internal/owner"
)

func sqlStore(t *testing.T, migrate bool) owner.SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if migrate {
		if _, err := db.Exec(owner.Migration); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return owner.SQLStore{DB: db}
}

func TestAVerdictSurvivesTheRoundTrip(t *testing.T) {
	s := sqlStore(t, true)
	want := owner.Validation{
		Build: "run-1", Attempt: 2, Verdict: owner.VerdictTestsGamed,
		Commit: "sha-a", Notes: "assert_true(True) in test_valid_url",
	}
	if err := s.Upsert(context.Background(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.Find(context.Background(), "run-1", 2)
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got != want {
		t.Errorf("read back %+v", got)
	}
}

func TestAVerdictDoesNotCarryToAnotherAttempt(t *testing.T) {
	// A rerun after integration needs a FRESH pass: the code changed, so a
	// verdict from the previous attempt says nothing about this one.
	s := sqlStore(t, true)
	if err := s.Upsert(context.Background(), owner.Validation{
		Build: "run-1", Attempt: 1, Verdict: owner.VerdictSatisfied, Commit: "sha-a",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, found, err := s.Find(context.Background(), "run-1", 2)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found {
		t.Error("attempt 1's verdict stood in for attempt 2")
	}
}

func TestRevalidatingAnAttemptReplacesItsVerdict(t *testing.T) {
	s := sqlStore(t, true)
	v := owner.Validation{
		Build: "run-1", Attempt: 1, Verdict: owner.VerdictNotSatisfied,
		Commit: "sha-a", Notes: "AC-two has no test",
	}
	if err := s.Upsert(context.Background(), v); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	v.Verdict, v.Notes = owner.VerdictSatisfied, "AC-two now has one"
	if err := s.Upsert(context.Background(), v); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, _, err := s.Find(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !got.Passed() || got.Notes != "AC-two now has one" {
		t.Errorf("read back %+v", got)
	}
}

func TestARunWithNoVerdictIsNotFound(t *testing.T) {
	s := sqlStore(t, true)
	_, found, err := s.Find(context.Background(), "run-absent", 1)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found {
		t.Error("a run that was never validated was found")
	}
}

func TestAValidationStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	s := sqlStore(t, false)
	if _, _, err := s.Find(context.Background(), "run-1", 1); err == nil {
		t.Error("a missing table read as a run with no verdict")
	}
	if err := s.Upsert(context.Background(), owner.Validation{Build: "run-1"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
}
