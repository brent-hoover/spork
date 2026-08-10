package identity_test

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"sutra/internal/identity"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=busy_timeout(5000)", t.Name()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := identity.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestNewUUIDv7Version(t *testing.T) {
	id := identity.NewUUIDv7()
	if len(id) != 36 {
		t.Fatalf("uuid length: %q", id)
	}
	if id[14] != '7' {
		t.Fatalf("expected UUID version 7 per CON-uuid-keys, got version %c in %q", id[14], id)
	}
	variant := id[19]
	if !strings.ContainsRune("89ab", rune(variant)) {
		t.Fatalf("expected RFC 9562 variant, got %c in %q", variant, id)
	}
}

func TestCreateTranslatesConstraintToDuplicate(t *testing.T) {
	db := openDB(t)
	tx1, _ := db.Begin()
	first, err := identity.Create(tx1, "claude", "agent", nil)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx2, _ := db.Begin()
	defer func() { _ = tx2.Rollback() }()
	_, err = identity.Create(tx2, "claude", "human", nil)
	var dup *identity.DuplicateHandleError
	if !errors.As(err, &dup) {
		t.Fatalf("expected DuplicateHandleError, got %v", err)
	}
	if dup.ExistingID != first.ID {
		t.Fatalf("duplicate names %s, want winner %s", dup.ExistingID, first.ID)
	}
}

// TestCreateConcurrentDuplicates races N transactions on one handle: the
// UNIQUE constraint is the authority, so exactly one wins and every
// loser's error names the winner.
func TestCreateConcurrentDuplicates(t *testing.T) {
	db := openDB(t)
	const n = 8
	var wg sync.WaitGroup
	winners := make(chan identity.Identity, n)
	losers := make(chan *identity.DuplicateHandleError, n)
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
			created, err := identity.Create(tx, "raced", "agent", nil)
			var dup *identity.DuplicateHandleError
			switch {
			case err == nil:
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
				winners <- created
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
	var won []identity.Identity
	for w := range winners {
		won = append(won, w)
	}
	if len(won) != 1 {
		t.Fatalf("expected exactly one winner, got %d", len(won))
	}
	for l := range losers {
		if l.ExistingID != won[0].ID {
			t.Fatalf("loser names %s, want winner %s", l.ExistingID, won[0].ID)
		}
	}
}

// TestCursorCloseReleasesTheConnection pins what Close is FOR.
//
// Nothing else here observes it. Every other use closes a cursor and then
// stops caring, so a Close that returns nil without releasing anything
// passes every test in this package and every scenario in the suite — the
// rows are simply never read again. What a leaked cursor does instead is
// hold its connection open, and open readers block writers, so the symptom
// arrives later and somewhere else as a stall rather than a failure.
//
// database/sql counts the connections in use, which turns "the handle was
// released" into something a test can assert directly: one is held while
// the cursor is open, and none after it is closed.
func TestCursorCloseReleasesTheConnection(t *testing.T) {
	db := openDB(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, handle := range []string{"claude", "human-brent"} {
		if _, err := identity.Create(tx, handle, "agent", nil); err != nil {
			t.Fatalf("create %s: %v", handle, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	cursor, err := identity.OpenList(db, "")
	if err != nil {
		t.Fatalf("open list: %v", err)
	}
	if got := db.Stats().InUse; got != 1 {
		t.Fatalf("an open cursor must hold its connection, InUse=%d", got)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := db.Stats().InUse; got != 0 {
		t.Fatalf("Close did not release the connection, InUse=%d", got)
	}
}
