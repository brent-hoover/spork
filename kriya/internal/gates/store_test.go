package gates_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"kriya/internal/gates"
)

func sqlGateStore(t *testing.T) gates.SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gates.Migration); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gates.SQLStore{DB: db}
}

func TestAPassIsOnlyAPassAtItsOwnCommit(t *testing.T) {
	// The commit is in the WHERE clause, which is what makes a stale pass
	// unusable rather than merely old.
	s := sqlGateStore(t)
	err := s.Upsert(context.Background(), gates.Result{
		Build: "run-1", Module: "engine", Gate: "test", Attempt: 1,
		Commit: "sha-a", Passed: true,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	at, err := s.Passed(context.Background(), "run-1", "engine", "test", "sha-a", 1)
	if err != nil {
		t.Fatalf("passed: %v", err)
	}
	if !at {
		t.Error("a pass recorded at sha-a did not satisfy at sha-a")
	}
	stale, err := s.Passed(context.Background(), "run-1", "engine", "test", "sha-b", 1)
	if err != nil {
		t.Fatalf("passed: %v", err)
	}
	if stale {
		t.Error("a pass from an older commit satisfied at a newer one")
	}
}

func TestAnAttemptsResultIsItsOwn(t *testing.T) {
	// Integrating a new base can leave the branch head unchanged: same commit,
	// different ground. A result from the previous attempt says nothing about
	// the new one, so each attempt is asked about separately.
	s := sqlGateStore(t)
	for attempt, passed := range map[int]bool{1: true, 2: false} {
		err := s.Upsert(context.Background(), gates.Result{
			Build: "run-1", Module: "engine", Gate: "test", Attempt: attempt,
			Commit: "sha-a", Passed: passed,
		})
		if err != nil {
			t.Fatalf("upsert attempt %d: %v", attempt, err)
		}
	}
	first, err := s.Passed(context.Background(), "run-1", "engine", "test", "sha-a", 1)
	if err != nil {
		t.Fatalf("passed: %v", err)
	}
	if !first {
		t.Error("attempt 1's pass was lost")
	}
	second, err := s.Passed(context.Background(), "run-1", "engine", "test", "sha-a", 2)
	if err != nil {
		t.Fatalf("passed: %v", err)
	}
	if second {
		t.Error("attempt 1's pass satisfied attempt 2")
	}
	// And an attempt that never ran has not passed.
	third, err := s.Passed(context.Background(), "run-1", "engine", "test", "sha-a", 3)
	if err != nil {
		t.Fatalf("passed: %v", err)
	}
	if third {
		t.Error("an attempt that never ran reported as passed")
	}
}

func TestReRecordingAnAttemptReplacesIt(t *testing.T) {
	s := sqlGateStore(t)
	result := gates.Result{
		Build: "run-1", Module: "engine", Gate: "test", Attempt: 1,
		Commit: "sha-a", Passed: false,
	}
	if err := s.Upsert(context.Background(), result); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	result.Passed = true
	if err := s.Upsert(context.Background(), result); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, err := s.Passed(context.Background(), "run-1", "engine", "test", "sha-a", 1)
	if err != nil {
		t.Fatalf("passed: %v", err)
	}
	if !got {
		t.Error("a re-run of the same attempt did not replace its result")
	}
}

func TestAGateNeverRunHasNotPassed(t *testing.T) {
	// Absence is not failure: the chain needs "has not passed", not an error.
	s := sqlGateStore(t)
	got, err := s.Passed(context.Background(), "run-1", "engine", "mutation", "sha-a", 1)
	if err != nil {
		t.Fatalf("passed: %v", err)
	}
	if got {
		t.Error("a gate that never ran reported as passed")
	}
}

func TestAWriteToAMissingTableFails(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	err = gates.SQLStore{DB: db}.Upsert(context.Background(), gates.Result{Build: "run-1"})
	if err == nil {
		t.Error("a write to a missing table reported success")
	}
}
