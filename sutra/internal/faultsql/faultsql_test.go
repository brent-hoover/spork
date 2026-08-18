package faultsql

import (
	"database/sql"
	"strings"
	"testing"
)

// open returns a handle on a private in-memory database through the fault
// driver, with the given fault parameters appended.
func open(t *testing.T, fault string) *sql.DB {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
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

// TestNoFaultParametersBehaveLikeTheRealDriver is the control. If the
// wrapper changed behaviour when asked for nothing, every test built on it
// would be measuring the wrapper rather than the code.
func TestNoFaultParametersBehaveLikeTheRealDriver(t *testing.T) {
	db := open(t, "")
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (name) VALUES ('a'), ('b')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := db.Query(`SELECT name FROM t ORDER BY name`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("got %v", got)
	}
}

// TestFaultAfterHitsExactlyTheNthOperation is the property the whole sweep
// depends on: one call fails and its neighbours do not, so a test can name
// which error branch it is standing on.
func TestFaultAfterHitsExactlyTheNthOperation(t *testing.T) {
	db := open(t, "fault_op=exec&fault_after=3")
	// Two succeed.
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("first exec: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("second exec: %v", err)
	}
	// The third fails.
	_, err := db.Exec(`INSERT INTO t (id) VALUES (2)`)
	if err == nil {
		t.Fatal("the third exec was expected to fail")
	}
	if !strings.Contains(err.Error(), "injected fault") {
		t.Fatalf("not our error: %v", err)
	}
	// And the fourth succeeds again, which is what makes the fault a
	// scalpel rather than a broken database.
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (3)`); err != nil {
		t.Fatalf("fourth exec should have succeeded: %v", err)
	}
}

// TestFaultOpDoesNotCountOtherKinds pins that fault_op=commit&fault_after=1
// means "the first commit", not "the first operation, if it is a commit".
func TestFaultOpDoesNotCountOtherKinds(t *testing.T) {
	db := open(t, "fault_op=commit&fault_after=1")
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("exec should be untouched: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin should be untouched: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("exec in tx should be untouched: %v", err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("the commit was expected to fail")
	}
}

// TestFaultOnNextReachesTheIterationBranches covers the arms no
// query-level failure can: a cursor that yields a row and then breaks,
// which is what rows.Err() exists for.
func TestFaultOnNextReachesTheIterationBranches(t *testing.T) {
	// One handle: fault_op=next ignores the execs, so the fixture rows are
	// inserted normally and only the iteration breaks.
	db := open(t, "fault_op=next&fault_after=2")
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (1), (2), (3)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := db.Query(`SELECT id FROM t ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	if n != 1 {
		t.Fatalf("expected one row before the break, got %d", n)
	}
	if err := rows.Err(); err == nil {
		t.Fatal("rows.Err() was expected to report the injected fault")
	}
}

// TestRepeatKeepsFailing is for the paths that retry: a single failure
// would be swallowed by a retry loop and prove nothing.
func TestRepeatKeepsFailing(t *testing.T) {
	db := open(t, "fault_op=exec&fault_after=1&fault_repeat=1")
	for i := range 3 {
		if _, err := db.Exec(`SELECT 1`); err == nil {
			t.Fatalf("exec %d should have failed", i+1)
		}
	}
}

// TestUnknownParameterIsRefused matters more than it looks: a silently
// ignored typo produces a test that injects nothing and passes, which is
// the exact failure mode the coverage gate exists to refuse.
func TestUnknownParameterIsRefused(t *testing.T) {
	for _, dsn := range []string{
		"file:x?fault_afetr=2",
		"file:x?fault_op=nonsense",
		"file:x?fault_after=0",
		"file:x?fault_after=notanumber",
	} {
		db, err := sql.Open("sqlite-fault", dsn)
		if err == nil {
			// sql.Open is lazy; the parse happens on first use.
			err = db.Ping()
			_ = db.Close()
		}
		if err == nil {
			t.Fatalf("%q was accepted", dsn)
		}
	}
}

// TestRealDSNParametersSurvive pins that the sqlite pragmas the app relies
// on still reach the real driver after the fault parameters are stripped.
func TestRealDSNParametersSurvive(t *testing.T) {
	db, err := sql.Open("sqlite-fault",
		"file:"+t.Name()+"?mode=memory&cache=shared&_pragma=busy_timeout(5000)&fault_after=99&fault_op=exec")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var timeout int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("read pragma: %v", err)
	}
	if timeout != 5000 {
		t.Fatalf("busy_timeout did not survive the DSN rewrite: got %d", timeout)
	}
}

// TestBadRowMakesScanFail covers the arms a query-level failure cannot
// reach: the `err != nil` after rows.Scan, which needs a row that arrives
// and then refuses to be read.
func TestBadRowMakesScanFail(t *testing.T) {
	db := open(t, "fault_op=badrow&fault_after=1")
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (id, name) VALUES (1, 'a')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := db.Query(`SELECT id, name FROM t`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("expected a row; rows.Err=%v", rows.Err())
	}
	var id int
	var name string
	if err := rows.Scan(&id, &name); err == nil {
		t.Fatal("Scan was expected to fail on the unscannable row")
	}
}
