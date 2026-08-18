// Package faultsql registers a database/sql driver that fails on demand,
// so the error branches of every store call can be exercised.
//
// Sutra's uncovered conditional arms are overwhelmingly `err != nil` after
// a database call: 791 of them, and no test can reach one while the
// database always succeeds. This makes the failure the test's choice.
//
// It is controlled ENTIRELY through the DSN and exports nothing. That is
// deliberate: an exported setter here would be a function no production
// path calls, which the deadcode gate correctly reports as unreachable
// from main — the same trap the export_test.go hooks fell into. A blank
// import plus a query parameter leaves no such surface.
//
//	import _ "sutra/internal/faultsql"
//
//	db, _ := sql.Open("sqlite-fault",
//	    "file:x?mode=memory&cache=shared&fault_after=7")
//
// Parameters, all stripped before the DSN reaches the real driver:
//
//	fault_after=N   fail the Nth matching operation (1-based). Every
//	                operation before and after it succeeds, so a test
//	                pinpoints one call rather than breaking everything.
//	fault_op=KIND   which operations count toward N and get to fail:
//	                any (default), query, exec, begin, commit, prepare,
//	                next (the cursor breaks mid-iteration), close,
//	                badrow (the Nth row arrives unscannable).
//	fault_repeat=1  keep failing every matching operation from the Nth
//	                on, rather than only the Nth.
//	fault_msg=TEXT  the error text (default "injected fault").
//
// The counter is per *sql.DB, not global, so parallel tests with their own
// handles cannot disturb each other.
package faultsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	"modernc.org/sqlite"
)

func init() { sql.Register("sqlite-fault", &faultDriver{}) }

// plan is the fault a DSN asked for, plus the running count of matching
// operations. One plan per Open, so it is per-handle state.
type plan struct {
	after  int64
	op     string
	repeat bool
	msg    string
	seen   atomic.Int64
}

// trip reports whether this operation is the one that should fail, and
// counts it. Operations of other kinds do not advance the counter, so
// fault_op=exec&fault_after=2 means "the second Exec", not "the second
// call, if it happens to be an Exec".
func (p *plan) trip(kind string) bool {
	if p == nil || p.after <= 0 {
		return false
	}
	if p.op != "any" && p.op != kind {
		return false
	}
	n := p.seen.Add(1)
	return n == p.after || (p.repeat && n > p.after)
}

func (p *plan) err(kind string) error {
	return fmt.Errorf("%s: %s", kind, p.msg)
}

// parseDSN splits a DSN into the fault plan and the DSN the real driver
// should see. An unknown fault_* parameter is an ERROR rather than being
// ignored: a typo would otherwise silently produce a passing test that
// injected nothing.
func parseDSN(dsn string) (*plan, string, error) {
	i := strings.IndexByte(dsn, '?')
	if i < 0 {
		return &plan{}, dsn, nil
	}
	head, query := dsn[:i], dsn[i+1:]
	values, err := url.ParseQuery(query)
	if err != nil {
		return nil, "", fmt.Errorf("faultsql: parse DSN query: %w", err)
	}
	p := &plan{op: "any", msg: "injected fault"}
	for key, vals := range values {
		if !strings.HasPrefix(key, "fault_") {
			continue
		}
		v := vals[len(vals)-1]
		switch key {
		case "fault_after":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return nil, "", fmt.Errorf("faultsql: fault_after must be a positive integer, got %q", v)
			}
			p.after = int64(n)
		case "fault_op":
			switch v {
			case "any", "query", "exec", "begin", "commit", "prepare", "next", "close", "badrow":
				p.op = v
			default:
				return nil, "", fmt.Errorf("faultsql: unknown fault_op %q", v)
			}
		case "fault_repeat":
			p.repeat = v == "1" || v == "true"
		case "fault_msg":
			p.msg = v
		default:
			return nil, "", fmt.Errorf("faultsql: unknown parameter %q", key)
		}
		delete(values, key)
	}
	rest := values.Encode()
	if rest == "" {
		return p, head, nil
	}
	return p, head + "?" + rest, nil
}

type faultDriver struct{}

func (d *faultDriver) Open(dsn string) (driver.Conn, error) {
	p, real, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	c, err := (&sqlite.Driver{}).Open(real)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: c, plan: p}, nil
}

// faultConn wraps the real connection. Every method either fails on cue or
// delegates; nothing is reimplemented, so the semantics under test are the
// real driver's.
type faultConn struct {
	driver.Conn
	plan *plan
}

func (c *faultConn) Begin() (driver.Tx, error) { //nolint:staticcheck // driver.Conn requires it
	if c.plan.trip("begin") {
		return nil, c.plan.err("begin")
	}
	tx, err := c.Conn.Begin() //nolint:staticcheck // delegating the deprecated path
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, plan: c.plan}, nil
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.plan.trip("begin") {
		return nil, c.plan.err("begin")
	}
	inner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("faultsql: underlying driver lacks ConnBeginTx")
	}
	tx, err := inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, plan: c.plan}, nil
}

func (c *faultConn) Prepare(query string) (driver.Stmt, error) {
	if c.plan.trip("prepare") {
		return nil, c.plan.err("prepare")
	}
	st, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &faultStmt{Stmt: st, plan: c.plan}, nil
}

func (c *faultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if c.plan.trip("prepare") {
		return nil, c.plan.err("prepare")
	}
	inner, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Prepare(query)
	}
	st, err := inner.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &faultStmt{Stmt: st, plan: c.plan}, nil
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.plan.trip("query") {
		return nil, c.plan.err("query")
	}
	inner, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := inner.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &faultRows{Rows: rows, plan: c.plan, badrow: c.plan.trip("badrow")}, nil
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.plan.trip("exec") {
		return nil, c.plan.err("exec")
	}
	inner, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return inner.ExecContext(ctx, query, args)
}

type faultTx struct {
	driver.Tx
	plan *plan
}

func (t *faultTx) Commit() error {
	if t.plan.trip("commit") {
		return t.plan.err("commit")
	}
	return t.Tx.Commit()
}

type faultStmt struct {
	driver.Stmt
	plan *plan
}

func (s *faultStmt) Query(args []driver.Value) (driver.Rows, error) { //nolint:staticcheck // driver.Stmt requires it
	if s.plan.trip("query") {
		return nil, s.plan.err("query")
	}
	rows, err := s.Stmt.Query(args) //nolint:staticcheck // delegating the deprecated path
	if err != nil {
		return nil, err
	}
	return &faultRows{Rows: rows, plan: s.plan, badrow: s.plan.trip("badrow")}, nil
}

func (s *faultStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if s.plan.trip("query") {
		return nil, s.plan.err("query")
	}
	inner, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := inner.QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return &faultRows{Rows: rows, plan: s.plan, badrow: s.plan.trip("badrow")}, nil
}

func (s *faultStmt) Exec(args []driver.Value) (driver.Result, error) { //nolint:staticcheck // driver.Stmt requires it
	if s.plan.trip("exec") {
		return nil, s.plan.err("exec")
	}
	return s.Stmt.Exec(args) //nolint:staticcheck // delegating the deprecated path
}

func (s *faultStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if s.plan.trip("exec") {
		return nil, s.plan.err("exec")
	}
	inner, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return inner.ExecContext(ctx, args)
}

// faultRows is what reaches the `rows.Err()` and mid-iteration scan
// branches: a cursor that yields some rows and then fails is the only way
// to exercise them, and it is a real failure mode — a dropped connection
// part-way through a large listing.
type faultRows struct {
	driver.Rows
	plan   *plan
	badrow bool
}

func (r *faultRows) Next(dest []driver.Value) error {
	if r.plan.trip("next") {
		return r.plan.err("next")
	}
	if r.badrow {
		// Columns() advertised one column more than the query has, so
		// dest is one longer than the real cursor will fill. Filling the
		// real ones and leaving the phantom nil makes rows.Scan fail its
		// arity check — which is what an `err != nil` after Scan is for.
		// A row that cannot be read into its destination is not
		// hypothetical: it is what a type drifting under a migration
		// looks like from here.
		if err := r.Rows.Next(dest[:len(dest)-1]); err != nil {
			return err
		}
		dest[len(dest)-1] = nil
		return nil
	}
	return r.Rows.Next(dest)
}

// Columns reports a phantom trailing column when a bad row was asked for,
// so the mismatch is established before the first Next.
func (r *faultRows) Columns() []string {
	cols := r.Rows.Columns()
	if r.badrow {
		return append(append([]string{}, cols...), "faultsql_phantom")
	}
	return cols
}

func (r *faultRows) Close() error {
	if r.plan.trip("close") {
		return r.plan.err("close")
	}
	return r.Rows.Close()
}
