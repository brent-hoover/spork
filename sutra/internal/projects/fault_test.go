package projects_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "sutra/internal/faultsql"
	"sutra/internal/projects"
)

func handle(t *testing.T, suffix, fault string) *sql.DB {
	t.Helper()
	dsn := "file:" + t.Name() + suffix + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)"
	if fault != "" {
		dsn += "&" + fault
	}
	h, err := sql.Open("sqlite-fault", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// armed migrates, creates one project, and returns a second handle on the
// same shared cache with the fault active — so setup cannot consume it.
func armed(t *testing.T, fault string) (*sql.DB, string) {
	t.Helper()
	setup := handle(t, "", "")
	if err := projects.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := setup.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	p, err := projects.Create(tx, projects.New{Key: "SEED", Name: "Seed"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return handle(t, "", fault), p.ID
}

// projects.go:78 — Migrate's `err != nil`.
func TestMigrateReportsAFailedExec(t *testing.T) {
	h := handle(t, "", "fault_op=exec&fault_after=1")
	if err := projects.Migrate(h); err == nil {
		t.Fatal("Migrate reported success on a failing exec")
	} else if !strings.Contains(err.Error(), "migrate projects") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// projects.go:102 — Create's unique-violation test taking its FALSE side:
// the insert failed for a reason that is not a duplicate key. The suite had
// only ever produced duplicates.
func TestCreateReportsANonDuplicateFailure(t *testing.T) {
	setup := handle(t, "", "")
	if err := projects.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := handle(t, "", "fault_op=exec&fault_after=1")
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = projects.Create(tx, projects.New{Key: "NEW", Name: "New"})
	if err == nil {
		t.Fatal("Create reported success on a failing insert")
	}
	var dup *projects.DuplicateKeyError
	if errors.As(err, &dup) {
		t.Fatal("a non-duplicate failure took the duplicate path")
	}
	if !strings.Contains(err.Error(), "insert project") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// projects.go:106 — Create's `scanErr != nil`: a real duplicate, whose
// resolution query then failed. An unresolved duplicate must not be
// reported as a resolved one, because the caller reads ExistingID from it.
func TestCreateReportsAFailedDuplicateResolution(t *testing.T) {
	h, _ := armed(t, "")
	_ = h
	setup := handle(t, "", "")
	tx, err := setup.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := projects.Create(tx, projects.New{Key: "TAKEN", Name: "Taken"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// The duplicate insert is an exec and lands honestly; the resolution
	// read is the first query this handle makes.
	faulty := handle(t, "", "fault_op=query&fault_after=1")
	tx2, err := faulty.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	_, err = projects.Create(tx2, projects.New{Key: "TAKEN", Name: "Again"})
	if err == nil {
		t.Fatal("Create reported success when the duplicate could not be resolved")
	}
	var dup *projects.DuplicateKeyError
	if errors.As(err, &dup) {
		t.Fatal("an unresolved duplicate must not be reported as a resolved one")
	}
	if !strings.Contains(err.Error(), "resolve duplicate key") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// projects.go:128 — Get's ErrNoRows arm.
func TestGetReportsNotFound(t *testing.T) {
	h, _ := armed(t, "")
	_, err := projects.Get(h, "99999999-9999-7999-8999-999999999999")
	var nf *projects.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected NotFoundError, got: %v", err)
	}
}

// projects.go:148 — OpenList's `err != nil`, and with it the Close of a
// cursor whose query never opened. That nil-rows path is live, not
// defensive: OpenList closes one on every failed open (review 1989).
func TestOpenListReportsAFailedQuery(t *testing.T) {
	h, _ := armed(t, "fault_op=query&fault_after=1")
	_, err := projects.OpenList(h, false)
	if err == nil {
		t.Fatal("OpenList reported success on a failing query")
	}
	if !strings.Contains(err.Error(), "list projects") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// projects.go:157, :160, :164 — Each's three arms.
func TestEachHandlesEveryFailure(t *testing.T) {
	t.Run("unscannable row", func(t *testing.T) {
		h, _ := armed(t, "fault_op=badrow&fault_after=1")
		cursor, err := projects.OpenList(h, true)
		if err != nil {
			t.Fatalf("open list: %v", err)
		}
		defer func() { _ = cursor.Close() }()
		if err := cursor.Each(func(projects.Project) error { return nil }); err == nil {
			t.Fatal("Each reported success on an unscannable row")
		}
	})
	t.Run("callback stops the stream", func(t *testing.T) {
		h, _ := armed(t, "")
		cursor, err := projects.OpenList(h, true)
		if err != nil {
			t.Fatalf("open list: %v", err)
		}
		defer func() { _ = cursor.Close() }()
		sentinel := errors.New("caller stopped")
		if err := cursor.Each(func(projects.Project) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
	t.Run("cursor breaks mid-stream", func(t *testing.T) {
		h, _ := armed(t, "fault_op=next&fault_after=1")
		cursor, err := projects.OpenList(h, true)
		if err != nil {
			t.Fatalf("open list: %v", err)
		}
		defer func() { _ = cursor.Close() }()
		err = cursor.Each(func(projects.Project) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "iterate projects") {
			t.Fatalf("expected the iteration failure, got: %v", err)
		}
	})
}

// projects.go:178, :182, :190, :199 — Archive's four. The UPDATE is
// conditional on archived_at IS NULL, so a zero-row result is ambiguous
// until a second read says which of three things happened; each of those
// three is a different answer to the caller.
func TestArchiveResolvesAZeroRowUpdate(t *testing.T) {
	t.Run("failed update", func(t *testing.T) {
		h, id := armed(t, "fault_op=exec&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = projects.Archive(tx, id)
		if err == nil || !strings.Contains(err.Error(), "archive project") {
			t.Fatalf("expected the update failure, got: %v", err)
		}
	})

	t.Run("already archived", func(t *testing.T) {
		h, id := armed(t, "")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := projects.Archive(tx, id); err != nil {
			t.Fatalf("first archive: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		tx2, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx2.Rollback() }()
		_, err = projects.Archive(tx2, id)
		var already *projects.AlreadyArchivedError
		if !errors.As(err, &already) {
			t.Fatalf("expected AlreadyArchivedError, got: %v", err)
		}
	})

	t.Run("no such project", func(t *testing.T) {
		h, _ := armed(t, "")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = projects.Archive(tx, "99999999-9999-7999-8999-999999999999")
		var nf *projects.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected NotFoundError, got: %v", err)
		}
	})

	t.Run("resolution itself fails", func(t *testing.T) {
		// The UPDATE affects nothing because the project is already
		// archived, and the read that would say so then fails. The caller
		// must be told the truth is unknown rather than given one of the
		// two confident answers.
		h, id := armed(t, "")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := projects.Archive(tx, id); err != nil {
			t.Fatalf("first archive: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		faulty := handle(t, "", "fault_op=query&fault_after=1")
		tx2, err := faulty.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx2.Rollback() }()
		_, err = projects.Archive(tx2, id)
		if err == nil {
			t.Fatal("Archive reported success when the resolution failed")
		}
		var already *projects.AlreadyArchivedError
		var nf *projects.NotFoundError
		if errors.As(err, &already) || errors.As(err, &nf) {
			t.Fatalf("an unresolved zero-row update must not become a confident answer: %v", err)
		}
		if !strings.Contains(err.Error(), "resolve archive of") {
			t.Fatalf("error lost its context: %v", err)
		}
	})

	t.Run("read-back fails", func(t *testing.T) {
		// The UPDATE lands, and the read that returns the archived row
		// fails. projects.go:199.
		h, id := armed(t, "fault_op=query&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = projects.Archive(tx, id)
		if err == nil {
			t.Fatal("Archive reported success when the read-back failed")
		}
		if !strings.Contains(err.Error(), "read archived project") {
			t.Fatalf("expected the read-back failure, got: %v", err)
		}
	})
}

// projects.go:212, :214 — IsArchived's two arms. This is the guard every
// project-scoped mutation consults, so confusing "no such project" with
// "the database broke" is how a write guard fails open.
func TestIsArchivedSeparatesAbsenceFromFailure(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		h, _ := armed(t, "")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = projects.IsArchived(tx, "99999999-9999-7999-8999-999999999999")
		var nf *projects.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected NotFoundError, got: %v", err)
		}
	})
	t.Run("failed query", func(t *testing.T) {
		h, id := armed(t, "fault_op=query&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		archived, err := projects.IsArchived(tx, id)
		if err == nil {
			t.Fatal("IsArchived reported success on a failing query")
		}
		if archived {
			t.Fatal("IsArchived reported true alongside an error")
		}
		var nf *projects.NotFoundError
		if errors.As(err, &nf) {
			t.Fatal("a query failure must not be reported as not-found")
		}
		if !strings.Contains(err.Error(), "check archive of") {
			t.Fatalf("error lost its context: %v", err)
		}
	})
}

// projects.go:128 — GetTx's ErrNoRows arm. Distinct code from Get's, which
// is why covering one left the other standing.
func TestGetTxReportsNotFound(t *testing.T) {
	h, _ := armed(t, "")
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = projects.GetTx(tx, "99999999-9999-7999-8999-999999999999")
	var nf *projects.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected NotFoundError, got: %v", err)
	}
}

// projects.go:182 — Archive's `err != nil` after res.RowsAffected(). The
// UPDATE succeeded; what failed was asking how many rows it changed, and
// the whole zero-row resolution below hangs off that number. SQLite never
// fails this, which is why the arm needed the injector's rowsaffected mode
// rather than a test — and why deleting the check would have been wrong:
// database/sql's Result contract permits the failure, so a driver swap
// would silently feed the resolution a zero it never earned.
func TestArchiveReportsAFailedRowCount(t *testing.T) {
	h, id := armed(t, "fault_op=rowsaffected&fault_after=1")
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = projects.Archive(tx, id)
	if err == nil {
		t.Fatal("Archive reported success when the row count was unavailable")
	}
	var already *projects.AlreadyArchivedError
	var nf *projects.NotFoundError
	if errors.As(err, &already) || errors.As(err, &nf) {
		t.Fatalf("an unavailable row count must not become a confident answer: %v", err)
	}
	if !strings.Contains(err.Error(), "archive project") {
		t.Fatalf("error lost its context: %v", err)
	}
}
