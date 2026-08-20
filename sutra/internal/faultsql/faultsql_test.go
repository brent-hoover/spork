package faultsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
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

// TestOnePlanServesTheWholePool is the property review 2090 found missing.
// driver.Open runs once per PHYSICAL connection, so a plan built there gives
// every pooled connection its own counter and "the second query" becomes
// whichever connection happened to serve it. With several connections open
// concurrently, exactly one query must fail — not one per connection, and
// not zero.
func TestOnePlanServesTheWholePool(t *testing.T) {
	db := open(t, "fault_op=query&fault_after=3")
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Four connections HELD at once, rather than concurrent queries that
	// might all reuse one. Review 2094 was right that the concurrent
	// version only made several connections likely; holding *sql.Conn
	// values makes it certain, which is what the assertion needs.
	ctx := context.Background()
	conns := make([]*sql.Conn, 4)
	for i := range conns {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns[i] = c
		defer func() { _ = c.Close() }() //nolint:revive // held for the test's duration on purpose
	}

	// Two queries down each connection. A shared counter fails exactly one
	// of the eight; four independent counters would each need three, so
	// none of them would reach it.
	var failures int
	for round := range 2 {
		for i, c := range conns {
			var n int
			if err := c.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
				failures++
				t.Logf("round %d, connection %d took the fault", round, i)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("expected exactly one failing query across four held connections, got %d", failures)
	}
}

// TestInjectedCommitLeavesNoOpenTransaction covers the other half of the
// same review: database/sql treats a failed Commit as final and returns the
// connection to the pool, so an injected failure that skipped the real
// rollback would leave SQLite inside a transaction and lock out the next
// writer. That is a HANG, not a failure, which is the worst kind.
func TestInjectedCommitLeavesNoOpenTransaction(t *testing.T) {
	db := open(t, "fault_op=commit&fault_after=1")
	db.SetMaxOpenConns(1) // force the next write onto the same connection
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("the commit was expected to fail")
	}
	// The write that proves the lock was released. Before the fix this
	// blocked until busy_timeout expired.
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (2)`); err != nil {
		t.Fatalf("the connection was left inside a transaction: %v", err)
	}
	// And the rolled-back row must be gone, which is what makes it a
	// rollback rather than a silent commit.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatal("the failed commit left its row behind")
	}
}

// TestInjectedCloseStillReleasesTheCursor is the same hazard on the read
// side: a cursor the caller was told failed to close, but which really is
// still open, holds a reader and starves writers.
//
// Honest about its strength: this is a PROPERTY check, not a discriminating
// one. Removing the real Close from the injected path does not make it fail,
// because SQLite permits a write while a read statement is open on the SAME
// connection, and a single-connection pool is the only way to force the
// write onto the cursor's connection. I could not construct a case that
// fails, so the fix stands on the reasoning — database/sql will not retry a
// recordingRows is a driver.Rows that remembers whether it was closed and
// answers with the error it was given. A real SQLite cursor cannot play this
// part: the effect under test is "the wrapper called through", and SQLite
// permits a write while a read cursor is open on the SAME connection, so the
// abandoned-cursor version of this test passed with the call REMOVED — it
// asserted a lock that never existed. Proven by deleting r.Rows.Close() and
// watching it still pass (review 2093, a second time).
type recordingRows struct {
	closed int
	err    error
}

func (r *recordingRows) Columns() []string              { return []string{"id"} }
func (r *recordingRows) Next(dest []driver.Value) error { return io.EOF }
func (r *recordingRows) Close() error {
	r.closed++
	return r.err
}

// TestInjectedCloseStillClosesTheUnderlyingCursor pins the cleanup half:
// database/sql treats a failed Close as final and will not retry, so a
// wrapper that reports failure WITHOUT closing leaks the real cursor.
func TestInjectedCloseStillClosesTheUnderlyingCursor(t *testing.T) {
	inner := &recordingRows{}
	p := &plan{after: 1, op: "close", msg: "injected fault"}
	p.armed.Store(true)
	rows := &faultRows{Rows: inner, plan: p}

	err := rows.Close()
	if err == nil {
		t.Fatal("expected the injected close failure")
	}
	if !strings.Contains(err.Error(), "injected fault") {
		t.Fatalf("expected the injected error, got %v", err)
	}
	if inner.closed != 1 {
		t.Fatalf("the real cursor must be closed exactly once, was closed %d times", inner.closed)
	}
}

// TestInjectedCloseKeepsTheUnderlyingErrorVisible is the close-side twin of
// the commit path's join (review 2094). A driver.ErrBadConn from the real
// Close is how database/sql learns to DISCARD the connection rather than
// pool it; replacing it with the injected error hands back a broken one.
func TestInjectedCloseKeepsTheUnderlyingErrorVisible(t *testing.T) {
	inner := &recordingRows{err: driver.ErrBadConn}
	p := &plan{after: 1, op: "close", msg: "injected fault"}
	p.armed.Store(true)
	rows := &faultRows{Rows: inner, plan: p}

	err := rows.Close()
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("the underlying ErrBadConn must survive the join, got %v", err)
	}
	if !strings.Contains(err.Error(), "injected fault") {
		t.Fatalf("the injected error must survive too, got %v", err)
	}
}

// TestBadRowSelectsAResultSetNotARow pins the semantics review 2090 caught
// the documentation overstating. The column mismatch that makes Scan fail is
// settled by Columns() before the first Next, so badrow can only select a
// QUERY: with fault_after=2, the first query's rows read fine and the
// second's do not.
func TestBadRowSelectsAResultSetNotARow(t *testing.T) {
	db := open(t, "fault_op=badrow&fault_after=2")
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (1), (2)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	scanAll := func() error {
		rows, err := db.Query(`SELECT id FROM t ORDER BY id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := scanAll(); err != nil {
		t.Fatalf("the first query should have been untouched: %v", err)
	}
	if err := scanAll(); err == nil {
		t.Fatal("the second query's rows should have been unscannable")
	}
	if err := scanAll(); err != nil {
		t.Fatalf("the third query should have been untouched again: %v", err)
	}
}

// TestRowsAffectedCanFail covers the mode that exists for one reason:
// database/sql's Result contract allows RowsAffected to return an error and
// SQLite never does, so every `err != nil` after it is unreachable without
// help. Deleting those checks instead would leave a real driver's failure
// silently ignored.
func TestRowsAffectedCanFail(t *testing.T) {
	db := open(t, "fault_op=rowsaffected&fault_after=1")
	// The Exec itself succeeds — only the question about it fails, which is
	// the shape the guarded code has to cope with.
	res, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	if err != nil {
		t.Fatalf("the exec itself should have succeeded: %v", err)
	}
	if _, err := res.RowsAffected(); err == nil {
		t.Fatal("RowsAffected was expected to fail")
	}
	// And the next one is unaffected, so it is one call rather than a
	// broken database.
	res2, err := db.Exec(`INSERT INTO t (id) VALUES (1)`)
	if err != nil {
		t.Fatalf("the second exec should have succeeded: %v", err)
	}
	if n, err := res2.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("the second RowsAffected should have been untouched: n=%d err=%v", n, err)
	}
}

// TestZeroRowsReportsNoChange covers the mode that exists for the
// compare-and-set branches: a statement that succeeded and changed nothing,
// which every CAS here reads as a lost race. Winning a real race against
// yourself is not a test one can write.
func TestZeroRowsReportsNoChange(t *testing.T) {
	// Ordinal one is the first RowsAffected CALL, not the first Exec — the
	// CREATE's result is never asked for its count, so it spends nothing.
	db := open(t, "fault_op=zerorows&fault_after=1")
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := db.Exec(`INSERT INTO t (id) VALUES (1)`)
	if err != nil {
		t.Fatalf("the exec itself should have succeeded: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("zerorows must not report an error, only a zero: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected zero rows affected, got %d", n)
	}
	// The row really was inserted — the lie is only in the count, which is
	// what makes this a race simulation rather than a broken write.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("the insert should still have happened, found %d rows", count)
	}
}

// TestArmOnDefersTheCounter covers the mechanism that makes the HTTP-level
// sweep possible: setup and the code under test share a handle, so the
// counter has to start after setup rather than at the first operation.
func TestArmOnDefersTheCounter(t *testing.T) {
	db := open(t, "fault_op=exec&fault_after=1&fault_arm_on=ARMED")
	// Setup, all of it uncounted.
	for _, stmt := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY)`,
		`INSERT INTO t (id) VALUES (1)`,
		`INSERT INTO t (id) VALUES (2)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup %q should be uncounted and unfaulted: %v", stmt, err)
		}
	}
	// The marker itself is checked, never counted, so it must not fail.
	if _, err := db.Exec(`SELECT 1 /* ARMED */`); err != nil {
		t.Fatalf("the marker statement must not be the fault: %v", err)
	}
	// And now the very next exec is ordinal one.
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (3)`); err == nil {
		t.Fatal("the first exec after arming should have failed")
	}
}

// TestAnyDoesNotSpendOrdinalsOnSyntheticModes pins a bug the HTTP-level
// sweep found in this driver. Every Exec consults the two Result modes, so
// counting them under fault_op=any made each exec spend THREE ordinals:
// "the third operation" meant the first, and a sweep walking ordinals
// measured a third of what it believed. The synthetic modes fire only when
// named.
func TestAnyDoesNotSpendOrdinalsOnSyntheticModes(t *testing.T) {
	db := open(t, "fault_op=any&fault_after=3")
	// begin, exec, exec — three real operations, so the third fails and
	// the first two do not.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin is operation one and must succeed: %v", err)
	}
	if _, err := tx.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("operation two must succeed: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO t (id) VALUES (1)`); err == nil {
		t.Fatal("operation three should have failed")
	}
	_ = tx.Rollback()
}

// TestArmMarkerSpendsNoOrdinal is the other half: the marker statement arms
// the counter and must not consume the first ordinal itself — not through
// the fault check, and not through its Result either.
func TestArmMarkerSpendsNoOrdinal(t *testing.T) {
	db := open(t, "fault_op=any&fault_after=1&fault_arm_on=MARK")
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("setup is uncounted: %v", err)
	}
	if _, err := db.Exec(`SELECT 1 /* MARK */`); err != nil {
		t.Fatalf("the marker must not fail: %v", err)
	}
	// Begin is now ordinal one.
	if _, err := db.Begin(); err == nil {
		t.Fatal("the first operation after the marker should have failed")
	}
}

// TestFiredIsObservable covers the mechanism a sweep needs to tell "the
// ordinal was past the end" from "the fault fired and the code swallowed
// it". Without it a swallowed error reads as success and the sweep stops
// early — which is exactly the regression such a sweep exists to catch.
func TestFiredIsObservable(t *testing.T) {
	db := open(t, "fault_op=exec&fault_after=2")
	fired := func() int64 {
		t.Helper()
		var n int64
		if err := db.QueryRow(`SELECT faultsql_fired()`).Scan(&n); err != nil {
			t.Fatalf("read fired: %v", err)
		}
		return n
	}
	if n := fired(); n != 0 {
		t.Fatalf("nothing has fired yet, got %d", n)
	}
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("first exec: %v", err)
	}
	if n := fired(); n != 0 {
		t.Fatalf("the first exec should not have fired, got %d", n)
	}
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (1)`); err == nil {
		t.Fatal("the second exec should have failed")
	}
	if n := fired(); n != 1 {
		t.Fatalf("exactly one fault should have fired, got %d", n)
	}
	// And the observation query itself is neither counted nor faulted, or
	// asking the question would change the answer.
	_ = fired()
	if n := fired(); n != 1 {
		t.Fatalf("asking must not change the count, got %d", n)
	}
}

// TestResultModesCountOnlyWhenTheCountIsRead pins the semantics reviews 2096
// and 2108 corrected. Tripping when Exec returned spent ordinals on results
// nobody reads — sutra's idempotency reservation ignores its own — so "the
// second row count" silently meant "the second Exec", and a sweep aimed at
// row-count failures landed on statements whose counts are never consulted.
func TestResultModesCountOnlyWhenTheCountIsRead(t *testing.T) {
	db := open(t, "fault_op=rowsaffected&fault_after=1")
	// Two execs whose results are DISCARDED. Under the old behaviour these
	// spent both ordinals and the fault fired here, invisibly.
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var fired int64
	if err := db.QueryRow(`SELECT faultsql_fired()`).Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 0 {
		t.Fatalf("results nobody read must not spend an ordinal, but %d fired", fired)
	}
	// The first count actually ASKED FOR is ordinal one.
	res, err := db.Exec(`INSERT INTO t (id) VALUES (2)`)
	if err != nil {
		t.Fatalf("third exec: %v", err)
	}
	if _, err := res.RowsAffected(); err == nil {
		t.Fatal("the first row count read should have failed")
	}
}

// TestMarkerSpendsNoOrdinalThroughAnyDoor is review 2107's second finding.
// arm() fires on the statement TEXT, so it can arrive through Exec, Query,
// or a prepared statement — but only the Exec door suppressed the fault
// checks. Through the other doors the marker armed the counter and then
// immediately spent ordinals on its own rows, and a PREPARED marker tripped
// on every execution: the statement whose whole job is to schedule a fault
// was injecting it instead.
func TestMarkerSpendsNoOrdinalThroughAnyDoor(t *testing.T) {
	for _, door := range []string{"query", "prepared-query", "prepared-exec"} {
		t.Run(door, func(t *testing.T) {
			// fault_after=1 on "any": if the marker spends ANYTHING, it
			// spends the ordinal meant for the operation under test.
			db := open(t, "fault_op=any&fault_after=1&fault_arm_on=ARMNOW")
			if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
				t.Fatalf("create: %v", err)
			}
			marker := `SELECT 1 /* ARMNOW */`
			switch door {
			case "query":
				rows, err := db.Query(marker)
				if err != nil {
					t.Fatalf("marker query: %v", err)
				}
				for rows.Next() { // spends next ordinals if wrapped
					var n int
					if err := rows.Scan(&n); err != nil {
						t.Fatalf("the marker's own rows were corrupted: %v", err)
					}
				}
				if err := rows.Close(); err != nil {
					t.Fatalf("marker close: %v", err)
				}
			case "prepared-query":
				st, err := db.Prepare(marker)
				if err != nil {
					t.Fatalf("prepare marker: %v", err)
				}
				defer st.Close()
				for i := 0; i < 3; i++ { // a prepared marker run repeatedly
					rows, err := st.Query()
					if err != nil {
						t.Fatalf("prepared marker query %d: %v", i, err)
					}
					for rows.Next() {
						var n int
						if err := rows.Scan(&n); err != nil {
							t.Fatalf("prepared marker row %d: %v", i, err)
						}
					}
					if err := rows.Close(); err != nil {
						t.Fatalf("prepared marker close %d: %v", i, err)
					}
				}
			case "prepared-exec":
				st, err := db.Prepare(marker)
				if err != nil {
					t.Fatalf("prepare marker: %v", err)
				}
				defer st.Close()
				for i := 0; i < 3; i++ {
					if _, err := st.Exec(); err != nil {
						t.Fatalf("prepared marker exec %d: %v", i, err)
					}
				}
			}

			var fired int64
			if err := db.QueryRow(`SELECT faultsql_fired()`).Scan(&fired); err != nil {
				t.Fatalf("read fired: %v", err)
			}
			if fired != 0 {
				t.Fatalf("the marker fired %d faults; it must fire none", fired)
			}
			// The ordinal it must NOT have spent still belongs to the
			// first real operation after it.
			if _, err := db.Exec(`INSERT INTO t (id) VALUES (1)`); err == nil {
				t.Fatal("the marker consumed the ordinal meant for the first real operation")
			}
		})
	}
}

// legacyConn implements ONLY driver.Conn — no ConnPrepareContext. SQLite's
// driver has the contextual method, so database/sql never reaches the
// deprecated Prepare path through a real handle; a connection that lacks it
// is the only way to drive that door.
type legacyConn struct {
	prepared []string
}

func (c *legacyConn) Prepare(query string) (driver.Stmt, error) {
	c.prepared = append(c.prepared, query)
	return legacyStmt{}, nil
}
func (c *legacyConn) Close() error              { return nil }
func (c *legacyConn) Begin() (driver.Tx, error) { return nil, io.EOF }

type legacyStmt struct{}

func (legacyStmt) Close() error  { return nil }
func (legacyStmt) NumInput() int { return 0 }
func (legacyStmt) Exec(args []driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (legacyStmt) Query(args []driver.Value) (driver.Rows, error) {
	return &recordingRows{}, nil
}

// TestMarkerSurvivesTheLegacyPrepareDoor is review 2112. Fixing the marker on
// PrepareContext left the deprecated Prepare — and PrepareContext's own
// fallback into it — still building a faultStmt that did not know it held the
// marker, so executing it injected the fault it exists to schedule.
func TestMarkerSurvivesTheLegacyPrepareDoor(t *testing.T) {
	p := &plan{after: 1, op: "any", msg: "injected fault", armOn: "ARMNOW"}
	c := &faultConn{Conn: &legacyConn{}, plan: p}

	// Straight through the deprecated door.
	st, err := c.Prepare(`SELECT 1 /* ARMNOW */`)
	if err != nil {
		t.Fatalf("prepare marker: %v", err)
	}
	if _, err := st.Exec(nil); err != nil { //nolint:staticcheck // exercising the deprecated path on purpose
		t.Fatalf("the marker injected its own fault: %v", err)
	}
	if got := p.fired.Load(); got != 0 {
		t.Fatalf("the marker fired %d faults through Prepare; it must fire none", got)
	}
}

// TestPrepareContextFallbackArmsOnce covers the same door reached the other
// way. PrepareContext arms, finds no contextual Prepare, and delegates — and
// delegating to the public Prepare armed and counted a SECOND time, so
// fault_op=prepare&fault_after=2 fired on the first statement.
func TestPrepareContextFallbackArmsOnce(t *testing.T) {
	p := &plan{after: 2, op: "prepare", msg: "injected fault"}
	p.armed.Store(true)
	c := &faultConn{Conn: &legacyConn{}, plan: p}

	if _, err := c.PrepareContext(context.Background(), `SELECT 1`); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if got := p.seen.Load(); got != 1 {
		t.Fatalf("one prepare must spend one ordinal, spent %d", got)
	}
	if _, err := c.PrepareContext(context.Background(), `SELECT 2`); err == nil {
		t.Fatal("the SECOND prepare should have failed")
	}
}
