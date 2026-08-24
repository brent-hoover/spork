package devloop_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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
	// The real migration text, split as the composition root splits it: a test
	// that rebuilt the DDL by hand would pass while production had different
	// columns, proving only that the test agrees with itself.
	for _, schema := range []string{devloop.Migration, devloop.RoundsMigration, devloop.CaptureMigration,
		devloop.SystemFileMigration, devloop.ResumeMigration,
		devloop.SequenceMigration} {
		for _, stmt := range strings.Split(schema, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migrate: %v", err)
			}
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

func TestASessionWithNoImportStateStoresTheDeclaredDefault(t *testing.T) {
	// The column is an enum. A zero-valued Session must not put "" in it, or
	// every later read finds a value the spec does not declare.
	s := sqlSessionStore(t)
	if err := s.Upsert(context.Background(), devloop.Session{Run: "run-1", Ticket: "T-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var state string
	if err := s.DB.QueryRow(
		`SELECT import_state FROM dev_session WHERE run = ?`, "run-1").Scan(&state); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if state != devloop.ImportNone {
		t.Errorf("stored %q, want %q", state, devloop.ImportNone)
	}
}

func TestASetImportStateIsStoredAsGiven(t *testing.T) {
	s := sqlSessionStore(t)
	if err := s.Upsert(context.Background(), devloop.Session{
		Run: "run-1", Ticket: "T-1", ImportState: devloop.ImportImporting,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	open, err := s.Importing(context.Background())
	if err != nil {
		t.Fatalf("importing: %v", err)
	}
	if len(open) != 1 || open[0].Run != "run-1" {
		t.Fatalf("importing listed %+v", open)
	}
}
