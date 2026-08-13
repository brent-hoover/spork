package projects_test

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"sutra/internal/projects"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=busy_timeout(5000)", t.Name()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := projects.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// listAll drains the streaming cursor the API itself lists through, so
// the archive-visibility assertions run over the production path rather
// than a slurping sibling nothing else called (review 1996).
func listAll(db *sql.DB, includeArchived bool) ([]projects.Project, error) {
	cursor, err := projects.OpenList(db, includeArchived)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close() }()
	out := []projects.Project{}
	if err := cursor.Each(func(p projects.Project) error {
		out = append(out, p)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func create(t *testing.T, db *sql.DB, key string) projects.Project {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	p, err := projects.Create(tx, projects.New{Key: key, Name: key})
	if err != nil {
		t.Fatalf("create %s: %v", key, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return p
}

func TestDefaultBranchDefaultsToMain(t *testing.T) {
	db := openDB(t)
	p := create(t, db, "SUT")
	if p.DefaultBranch == nil || *p.DefaultBranch != "main" {
		t.Fatalf("default_branch: %+v", p.DefaultBranch)
	}
}

// TestCreateConcurrentDuplicateKeys races N transactions on one key:
// the UNIQUE constraint is the authority, exactly one wins, every loser
// names the winner (AC-unique-violation).
func TestCreateConcurrentDuplicateKeys(t *testing.T) {
	db := openDB(t)
	const n = 8
	var wg sync.WaitGroup
	winners := make(chan projects.Project, n)
	losers := make(chan *projects.DuplicateKeyError, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := db.Begin()
			if err != nil {
				errs <- err
				return
			}
			p, err := projects.Create(tx, projects.New{Key: "RACED", Name: "raced"})
			var dup *projects.DuplicateKeyError
			switch {
			case err == nil:
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
				winners <- p
			case errors.As(err, &dup):
				_ = tx.Rollback()
				losers <- dup
			default:
				_ = tx.Rollback()
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(winners)
	close(losers)
	close(errs)
	for err := range errs {
		t.Fatalf("unexpected error: %v", err)
	}
	var won []projects.Project
	for w := range winners {
		won = append(won, w)
	}
	if len(won) != 1 {
		t.Fatalf("expected exactly one winner, got %d", len(won))
	}
	for l := range losers {
		if l.ExistingID != won[0].ID {
			t.Fatalf("loser names %s, want %s", l.ExistingID, won[0].ID)
		}
	}
}

func TestArchiveHidesWithoutDeleting(t *testing.T) {
	db := openDB(t)
	p := create(t, db, "OTH")

	tx, _ := db.Begin()
	archived, err := projects.Archive(tx, p.ID)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if archived.ArchivedAt == nil {
		t.Fatal("archived_at not stamped")
	}

	visible, err := listAll(db, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(visible) != 0 {
		t.Fatalf("archived project still listed: %+v", visible)
	}
	all, err := listAll(db, true)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("archived project deleted: %+v", all)
	}
	if got, err := projects.Get(db, p.ID); err != nil || got.ID != p.ID {
		t.Fatalf("archived project unreadable: %v", err)
	}

	tx2, _ := db.Begin()
	defer func() { _ = tx2.Rollback() }()
	guard, err := projects.IsArchived(tx2, p.ID)
	if err != nil || !guard {
		t.Fatalf("write guard: archived=%v err=%v", guard, err)
	}
}

func TestSecondArchiveConflicts(t *testing.T) {
	db := openDB(t)
	p := create(t, db, "SUT")
	tx, _ := db.Begin()
	if _, err := projects.Archive(tx, p.ID); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	tx2, _ := db.Begin()
	defer func() { _ = tx2.Rollback() }()
	_, err := projects.Archive(tx2, p.ID)
	var already *projects.AlreadyArchivedError
	if !errors.As(err, &already) {
		t.Fatalf("expected AlreadyArchivedError, got %v", err)
	}
}

func TestArchiveUnknownProject(t *testing.T) {
	db := openDB(t)
	tx, _ := db.Begin()
	defer func() { _ = tx.Rollback() }()
	_, err := projects.Archive(tx, "no-such-id")
	var notFound *projects.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
}
