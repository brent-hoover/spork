package devloop_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"kriya/internal/devloop"
)

func sqlSessionStore(t *testing.T) devloop.SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{devloop.Migration,
		"ALTER TABLE dev_session ADD COLUMN rounds INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE dev_session ADD COLUMN commits TEXT NOT NULL DEFAULT '[]'"} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return devloop.SQLStore{DB: db}
}

func TestASessionAndItsRoundsAreRecorded(t *testing.T) {
	s := sqlSessionStore(t)
	sess := devloop.Session{
		Run: "run-1", Ticket: "T-1", SessionID: "s-1", Model: "a-model",
		Rounds: 2, Commits: []string{"sha-a", "sha-b"}, Ended: true,
	}
	if err := s.Upsert(context.Background(), sess); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Re-upserting the same run replaces it rather than duplicating: the loop
	// writes the row before the agent runs and again after every round.
	sess.Rounds = 3
	if err := s.Upsert(context.Background(), sess); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	var rounds int
	var commits string
	err := s.DB.QueryRow(`SELECT rounds, commits FROM dev_session WHERE run = ?`, "run-1").
		Scan(&rounds, &commits)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rounds != 3 {
		t.Errorf("rounds is %d after the second write", rounds)
	}
	if commits != `["sha-a","sha-b"]` {
		t.Errorf("commits came back %s — a round and its commit must stay linked", commits)
	}
}

func TestASessionWriteToAMissingTableFails(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	err = devloop.SQLStore{DB: db}.Upsert(context.Background(), devloop.Session{Run: "run-1"})
	if err == nil {
		t.Error("a write to a missing table reported success")
	}
}
