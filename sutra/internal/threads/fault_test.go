package threads_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	_ "sutra/internal/faultsql"
	"sutra/internal/threads"
)

// internal/threads had no tests of its own — the acceptance suite drove it
// end to end, which reaches the happy paths and none of the failures. This
// file stands on the error branches: what the code does when the database
// breaks under it, mid-query and mid-stream.

// db opens a private in-memory database through the fault driver. A fault
// spec of "" arms nothing, so the same helper serves setup and injection.
func db(t *testing.T, name, fault string) *sql.DB {
	t.Helper()
	dsn := "file:" + t.Name() + name + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)"
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

// seeded migrates a database, inserts one thread anchored to an issue, and
// returns a SECOND handle on the same shared cache with the fault armed —
// so the fixtures cannot consume the injected failure.
func seeded(t *testing.T, fault string) (*sql.DB, string) {
	t.Helper()
	setup := db(t, "", "")
	if err := threads.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := setup.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	issue := "11111111-1111-7111-8111-111111111111"
	th, err := threads.Create(tx, "seeded", json.RawMessage(`[{"speaker":"claude","text":"hi"}]`),
		nil, threads.Anchor{Issue: &issue})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return db(t, "", fault), th.ID
}

// threads.go:69 — Migrate's `err != nil`.
func TestMigrateReportsAFailedExec(t *testing.T) {
	h := db(t, "", "fault_op=exec&fault_after=1")
	err := threads.Migrate(h)
	if err == nil {
		t.Fatal("Migrate reported success on a failing exec")
	}
	if !strings.Contains(err.Error(), "migrate threads") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// threads.go:91 — Create's `err != nil`. A transcript that cannot be
// stored must not come back as a thread that exists.
func TestCreateReportsAFailedInsert(t *testing.T) {
	setup := db(t, "", "")
	if err := threads.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	armed := db(t, "", "fault_op=exec&fault_after=1")
	tx, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	project := "22222222-2222-7222-8222-222222222222"
	_, err = threads.Create(tx, "doomed", json.RawMessage(`[]`), nil,
		threads.Anchor{Project: &project})
	if err == nil {
		t.Fatal("Create reported success on a failing insert")
	}
	if !strings.Contains(err.Error(), "insert thread") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// threads.go:103 and :106 — Get's two arms: an id that names nothing, and
// a query that failed. Conflating them would turn a broken database into a
// confident 404.
func TestGetSeparatesAbsenceFromFailure(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		armed, _ := seeded(t, "")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = threads.Get(tx, "33333333-3333-7333-8333-333333333333")
		var nf *threads.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected NotFoundError, got: %v", err)
		}
	})
	t.Run("failed query", func(t *testing.T) {
		armed, id := seeded(t, "fault_op=query&fault_after=1")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = threads.Get(tx, id)
		if err == nil {
			t.Fatal("Get reported success on a failing query")
		}
		var nf *threads.NotFoundError
		if errors.As(err, &nf) {
			t.Fatal("a query failure must not be reported as not-found")
		}
		if !strings.Contains(err.Error(), "get thread") {
			t.Fatalf("error lost its context: %v", err)
		}
	})
}

// threads.go:117 — SetAnchor's `err != nil` from the Get it starts with.
// The anchor must not move when the thread could not be read.
func TestSetAnchorReportsAFailedRead(t *testing.T) {
	armed, id := seeded(t, "fault_op=query&fault_after=1")
	tx, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	project := "44444444-4444-7444-8444-444444444444"
	if _, _, err := threads.SetAnchor(tx, id, threads.Anchor{Project: &project}); err == nil {
		t.Fatal("SetAnchor reported success when the thread could not be read")
	}
}

// threads.go:122 — SetAnchor's `err != nil` from the UPDATE, reached only
// after the read succeeds. Two operations, so the ordinal has to skip the
// read: the query lands, the exec fails.
func TestSetAnchorReportsAFailedUpdate(t *testing.T) {
	armed, id := seeded(t, "fault_op=exec&fault_after=1")
	tx, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	project := "55555555-5555-7555-8555-555555555555"
	_, _, err = threads.SetAnchor(tx, id, threads.Anchor{Project: &project})
	if err == nil {
		t.Fatal("SetAnchor reported success on a failing update")
	}
	if !strings.Contains(err.Error(), "set anchor of thread") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// threads.go:226, :232, :235 — queryThreadsEach's three arms: the query
// fails, a row arrives unscannable, and the callback stops the stream.
func TestListByIssueEachHandlesEveryFailure(t *testing.T) {
	issue := "11111111-1111-7111-8111-111111111111"

	t.Run("failed query", func(t *testing.T) {
		armed, _ := seeded(t, "fault_op=query&fault_after=1")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		err = threads.ListByIssueEach(tx, issue, func(threads.Thread) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "query threads") {
			t.Fatalf("expected the query failure, got: %v", err)
		}
	})

	t.Run("unscannable row", func(t *testing.T) {
		armed, _ := seeded(t, "fault_op=badrow&fault_after=1")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		err = threads.ListByIssueEach(tx, issue, func(threads.Thread) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "scan thread") {
			t.Fatalf("expected the scan failure, got: %v", err)
		}
	})

	t.Run("callback stops the stream", func(t *testing.T) {
		armed, _ := seeded(t, "")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		sentinel := errors.New("caller stopped")
		err = threads.ListByIssueEach(tx, issue, func(threads.Thread) error { return sentinel })
		if !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
}

// threads.go:203, :209, :212 — queryRefsEach's three, the projection's
// copy of the same shape. Separate code, so separately reachable.
func TestRefsByIssueEachHandlesEveryFailure(t *testing.T) {
	issue := "11111111-1111-7111-8111-111111111111"

	t.Run("failed query", func(t *testing.T) {
		armed, _ := seeded(t, "fault_op=query&fault_after=1")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		err = threads.RefsByIssueEach(tx, issue, func(threads.Ref) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "query thread refs") {
			t.Fatalf("expected the query failure, got: %v", err)
		}
	})

	t.Run("unscannable row", func(t *testing.T) {
		armed, _ := seeded(t, "fault_op=badrow&fault_after=1")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		err = threads.RefsByIssueEach(tx, issue, func(threads.Ref) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "scan thread ref") {
			t.Fatalf("expected the scan failure, got: %v", err)
		}
	})

	t.Run("callback stops the stream", func(t *testing.T) {
		armed, _ := seeded(t, "")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		sentinel := errors.New("caller stopped")
		err = threads.RefsByIssueEach(tx, issue, func(threads.Ref) error { return sentinel })
		if !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
}
