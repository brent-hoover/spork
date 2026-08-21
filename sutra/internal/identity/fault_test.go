package identity_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "sutra/internal/faultsql"
	"sutra/internal/identity"
)

// This file drives identity's ERROR branches — the `err != nil` arms that
// no test can reach while the database always succeeds. Each case names
// the arm it stands on, because a fault test that merely "covers a line"
// is indistinguishable from one that reached it for the wrong reason.
//
// Four of the arms here need no fault at all: they were simply never
// exercised. They are grouped last, and calling them out matters — a
// missing test and an unreachable branch look identical in a coverage
// report and cost very different amounts to fix.

// faultDB opens a private in-memory database through the fault driver.
// The fault applies from the first matching operation, so callers that
// need fixtures either seed them with an operation of a different kind or
// pick an ordinal past the setup.
func faultDB(t *testing.T, fault string) *sql.DB {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)"
	if fault != "" {
		dsn += "&" + fault
	}
	db, err := sql.Open("sqlite-fault", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seeded returns a migrated database holding one identity, with the fault
// armed only afterwards — so setup cannot consume the injected failure.
func seeded(t *testing.T, fault string) (*sql.DB, string) {
	t.Helper()
	db := faultDB(t, "")
	if err := identity.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := identity.Create(tx, "seeded", "agent", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// A second handle on the same shared-cache database, this one armed.
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)&" + fault
	armed, err := sql.Open("sqlite-fault", dsn)
	if err != nil {
		t.Fatalf("open armed: %v", err)
	}
	t.Cleanup(func() { _ = armed.Close() })
	return armed, id.ID
}

// identity.go:44 — Migrate's `err != nil`.
func TestMigrateReportsAFailedExec(t *testing.T) {
	db := faultDB(t, "fault_op=exec&fault_after=1")
	err := identity.Migrate(db)
	if err == nil {
		t.Fatal("Migrate reported success on a failing exec")
	}
	if !strings.Contains(err.Error(), "migrate identities") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// identity.go:62 — Create's `!IsUniqueViolation(err)`, the arm taken when
// the insert fails for any reason OTHER than a duplicate handle. The suite
// only ever produced duplicates, so this side had never been taken.
func TestCreateReportsANonDuplicateFailure(t *testing.T) {
	db := faultDB(t, "")
	if err := identity.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	armed, err := sql.Open("sqlite-fault",
		"file:"+t.Name()+"?mode=memory&cache=shared&fault_op=exec&fault_after=1")
	if err != nil {
		t.Fatalf("open armed: %v", err)
	}
	defer func() { _ = armed.Close() }()
	tx, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := identity.Create(tx, "whoever", "agent", nil); err == nil {
		t.Fatal("Create reported success on a failing insert")
	} else if !strings.Contains(err.Error(), "insert identity") {
		t.Fatalf("a non-duplicate failure took the duplicate path: %v", err)
	}
}

// identity.go:66 — Create's `scanErr != nil`: the insert hit a real
// duplicate, and the follow-up read that resolves WHICH identity holds the
// handle then failed. Two faults in one call, which is why the ordinal
// matters: the insert must succeed in failing, and the query after it must
// fail too.
func TestCreateReportsAFailedDuplicateResolution(t *testing.T) {
	db := faultDB(t, "")
	if err := identity.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := identity.Create(tx, "taken", "agent", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Arm the first QUERY. The duplicate insert is an exec, so it still
	// reaches the UNIQUE constraint honestly; the resolution query is the
	// first query this handle makes.
	armed, err := sql.Open("sqlite-fault",
		"file:"+t.Name()+"?mode=memory&cache=shared&fault_op=query&fault_after=1")
	if err != nil {
		t.Fatalf("open armed: %v", err)
	}
	defer func() { _ = armed.Close() }()
	tx2, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin armed: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	_, err = identity.Create(tx2, "taken", "agent", nil)
	if err == nil {
		t.Fatal("Create reported success when the duplicate could not be resolved")
	}
	if !strings.Contains(err.Error(), "resolve duplicate handle") {
		t.Fatalf("expected the resolution failure, got: %v", err)
	}
	var dup *identity.DuplicateHandleError
	if errors.As(err, &dup) {
		t.Fatal("an unresolved duplicate must not be reported as a resolved one")
	}
}

// identity.go:127 — Each's `err != nil` after Scan. Needs a row that
// arrives and then refuses to be read, which is what badrow supplies.
func TestEachReportsAFailedScan(t *testing.T) {
	armed, _ := seeded(t, "fault_op=badrow&fault_after=1")
	cursor, err := identity.OpenList(armed, "")
	if err != nil {
		t.Fatalf("open list: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	err = cursor.Each(func(identity.Identity) error { return nil })
	if err == nil {
		t.Fatal("Each reported success on an unscannable row")
	}
	if !strings.Contains(err.Error(), "scan identity") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// identity.go:134 — Each's `rows.Err() != nil`: the cursor breaks
// mid-iteration. Distinct from a failed open, and the only thing that
// separates "the listing ended" from "the listing was cut short".
func TestEachReportsACursorThatBreaksMidStream(t *testing.T) {
	armed, _ := seeded(t, "fault_op=next&fault_after=1")
	cursor, err := identity.OpenList(armed, "")
	if err != nil {
		t.Fatalf("open list: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	err = cursor.Each(func(identity.Identity) error { return nil })
	if err == nil {
		t.Fatal("Each reported success on a broken cursor")
	}
	if !strings.Contains(err.Error(), "iterate identities") {
		t.Fatalf("expected the iteration failure, got: %v", err)
	}
}

// identity.go:159 — Lookup's `err != nil` for a failure that is NOT
// ErrNoRows. The suite had only ever produced the not-found case.
func TestLookupReportsAFailedQuery(t *testing.T) {
	armed, id := seeded(t, "fault_op=query&fault_after=1")
	tx, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = identity.Lookup(tx, id)
	if err == nil {
		t.Fatal("Lookup reported success on a failing query")
	}
	var nf *identity.NotFoundError
	if errors.As(err, &nf) {
		t.Fatal("a query failure must not be reported as not-found")
	}
	if !strings.Contains(err.Error(), "lookup identity") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// identity.go:177 — Get's `err != nil`.
func TestGetReportsAFailedQuery(t *testing.T) {
	armed, id := seeded(t, "fault_op=query&fault_after=1")
	tx, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := identity.Get(tx, id); err == nil {
		t.Fatal("Get reported success on a failing query")
	} else if !strings.Contains(err.Error(), "get identity") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// identity.go:189 — Exists's `err != nil`, the arm distinct from its
// ErrNoRows case. Getting these two confused would turn a broken database
// into a confident "no such identity", which is how a write guard fails
// open.
func TestExistsReportsAFailedQueryRatherThanAbsence(t *testing.T) {
	armed, id := seeded(t, "fault_op=query&fault_after=1")
	tx, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := identity.Exists(tx, id)
	if err == nil {
		t.Fatal("Exists reported success on a failing query")
	}
	if ok {
		t.Fatal("Exists reported true alongside an error")
	}
	if !strings.Contains(err.Error(), "check identity") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// --- Arms that needed no fault, only a test -------------------------------
//
// These four were uncovered for a plainer reason: nothing had ever called
// them that way. Worth separating from the faults above, because a report
// that lumps them together makes the remaining work look harder than it is.

// identity.go:101 — OpenList's `kind != ""`, the filtered listing.
func TestOpenListFiltersByKind(t *testing.T) {
	db := faultDB(t, "")
	if err := identity.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := identity.Create(tx, "a-human", "human", nil); err != nil {
		t.Fatalf("create human: %v", err)
	}
	if _, err := identity.Create(tx, "an-agent", "agent", nil); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	cursor, err := identity.OpenList(db, "human")
	if err != nil {
		t.Fatalf("open list: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	var handles []string
	if err := cursor.Each(func(i identity.Identity) error {
		handles = append(handles, i.Handle)
		return nil
	}); err != nil {
		t.Fatalf("each: %v", err)
	}
	if len(handles) != 1 || handles[0] != "a-human" {
		t.Fatalf("kind filter did not scope the listing: %v", handles)
	}
}

// identity.go:130 — Each's `err != nil` from the CALLBACK, which is a
// caller abandoning the stream rather than a database failure.
func TestEachStopsWhenTheCallbackFails(t *testing.T) {
	armed, _ := seeded(t, "")
	cursor, err := identity.OpenList(armed, "")
	if err != nil {
		t.Fatalf("open list: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	sentinel := errors.New("caller stopped")
	err = cursor.Each(func(identity.Identity) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
	}
}

// identity.go:156 — Lookup's ErrNoRows arm.
func TestLookupReportsNotFoundForAnUnknownID(t *testing.T) {
	armed, _ := seeded(t, "")
	tx, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = identity.Lookup(tx, identity.NewUUIDv7())
	var nf *identity.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected NotFoundError, got: %v", err)
	}
}

// identity.go:76 — IsUniqueViolation's two arms: `err != nil` false for a
// nil error, and the message check false for an unrelated one.
func TestIsUniqueViolationRejectsNilAndUnrelatedErrors(t *testing.T) {
	if identity.IsUniqueViolation(nil) {
		t.Fatal("nil is not a unique violation")
	}
	if identity.IsUniqueViolation(errors.New("disk went away")) {
		t.Fatal("an unrelated error is not a unique violation")
	}
}

// identity.go:85 — RESOLVED BY DELETION, and this comment is the record.
//
// NewUUIDv7 used to check rand.Read's error and panic. That branch was the
// last uncovered arm in this package, and trying to cover it is what proved
// it could not be: crypto/rand.Read routes every failure through fatal()
// and panic("unreachable") before any non-nil return, so it can only ever
// return (len(b), nil). Confirmed three ways — the documented contract, the
// implementation at crypto/rand/rand.go:63-66, and empirically, by
// replacing rand.Reader with a failing one and watching the process take a
// runtime fatal rather than an error.
//
// Deleting a guard on a doc comment alone is the dupkeys.go mistake. This
// one is deleted on the implementation and a demonstration.
