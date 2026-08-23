package workspace_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"kriya/internal/workspace"
)

func sqlStore(t *testing.T) workspace.SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{workspace.Migration, workspace.RemovalMigration} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return workspace.SQLStore{DB: db}
}

func TestAWorkspaceSurvivesTheRoundTrip(t *testing.T) {
	s := sqlStore(t)
	want := workspace.Workspace{
		Run: "run-1", Ticket: "T-1", Path: "/w/T-1", Branch: "kriya/T-1/abcd1234",
		Base: "base-sha", State: workspace.StateCreated,
	}
	if err := s.Upsert(context.Background(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.Find(context.Background(), "run-1")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got != want {
		t.Errorf("read back %+v", got)
	}
}

func TestAnUnknownRunIsNotFoundRatherThanAnError(t *testing.T) {
	// The difference matters: not-found means create one, an error means the
	// store is unwell and creating a second worktree would be wrong.
	s := sqlStore(t)
	_, found, err := s.Find(context.Background(), "run-absent")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found {
		t.Error("a run that was never recorded was found")
	}
}

func TestUpsertReplacesRatherThanDuplicates(t *testing.T) {
	s := sqlStore(t)
	w := workspace.Workspace{Run: "run-1", Ticket: "T-1", State: workspace.StatePending}
	if err := s.Upsert(context.Background(), w); err != nil {
		t.Fatalf("upsert pending: %v", err)
	}
	w.State = workspace.StateCreated
	if err := s.Upsert(context.Background(), w); err != nil {
		t.Fatalf("upsert ready: %v", err)
	}
	pending, err := s.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("a promoted workspace is still listed pending: %+v", pending)
	}
}

func TestPendingListsOnlyWhatACrashLeftBehind(t *testing.T) {
	s := sqlStore(t)
	rows := []workspace.Workspace{
		{Run: "run-b", Ticket: "T-2", State: workspace.StatePending},
		{Run: "run-a", Ticket: "T-1", State: workspace.StatePending},
		{Run: "run-c", Ticket: "T-3", State: workspace.StateCreated},
	}
	for _, w := range rows {
		if err := s.Upsert(context.Background(), w); err != nil {
			t.Fatalf("upsert %s: %v", w.Run, err)
		}
	}
	got, err := s.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(got) != 2 || got[0].Run != "run-a" || got[1].Run != "run-b" {
		t.Fatalf("listed %+v — recovery replays in a declared order", got)
	}
}

func TestAStoreThatCannotBeReadIsNotAnEmptyStore(t *testing.T) {
	// A short list reads exactly like "nothing left to recover", so a failure
	// to read must never come back as one.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := workspace.SQLStore{DB: db}
	if _, err := s.Pending(context.Background()); err == nil {
		t.Error("a missing workspace table listed as no pending work")
	}
	if _, _, err := s.Find(context.Background(), "run-1"); err == nil {
		t.Error("a missing workspace table read as a run that does not exist")
	}
	if err := s.Upsert(context.Background(), workspace.Workspace{Run: "run-1"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
}
