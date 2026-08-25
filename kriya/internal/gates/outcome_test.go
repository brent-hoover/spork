package gates_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"kriya/internal/gates"
)

func TestOutcomeSeparatesNotRunFromFailed(t *testing.T) {
	// The chain treats them alike — neither is a pass — but a report that
	// could not tell them apart would misstate where a run is.
	store := sqlGateStore(t)
	if err := store.Upsert(context.Background(), gates.Result{
		Build: "run-1", Module: "MOD-api", Gate: "test", Commit: "C2",
		Attempt: 1, Passed: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.Upsert(context.Background(), gates.Result{
		Build: "run-1", Module: "MOD-api", Gate: "structure", Commit: "C2",
		Attempt: 1, Passed: false,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	for _, tc := range []struct {
		gate        string
		passed, ran bool
	}{
		{"test", true, true},
		{"structure", false, true},
		{"typing", false, false},
	} {
		passed, ran, err := store.Outcome(context.Background(),
			"run-1", "MOD-api", tc.gate, "C2", 1)
		if err != nil {
			t.Fatalf("outcome %s: %v", tc.gate, err)
		}
		if passed != tc.passed || ran != tc.ran {
			t.Errorf("%s: passed=%v ran=%v, want passed=%v ran=%v",
				tc.gate, passed, ran, tc.passed, tc.ran)
		}
	}
}

func TestAnUnreadableStoreIsNotAGateThatHasNotRun(t *testing.T) {
	// Passed swallows this deliberately — absence and failure are one answer
	// to the chain — but a report must not claim a position it could not read.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (gates.SQLStore{DB: db}).Outcome(context.Background(),
		"run-1", "MOD-api", "test", "C2", 1); err == nil {
		t.Fatal("an unreadable store read as a gate that has not run")
	}
}
