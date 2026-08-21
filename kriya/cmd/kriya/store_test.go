package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTemp(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite",
		"file:"+filepath.Join(t.TempDir(), "kriya.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	if err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	return n > 0
}

func TestMigrationsApplyInTheOrderGiven(t *testing.T) {
	db := openTemp(t)
	// CREATE INDEX, not a REFERENCES clause. SQLite happily creates a foreign
	// key pointing at a table that does not exist yet — resolution is deferred
	// — so the obvious version of this test passes even when the order is
	// reversed and proves nothing. CREATE INDEX fails immediately on a missing
	// table, so this one genuinely detects reordering.
	ms := []migration{
		{module: "first", name: "0001_a", stmts: []string{`CREATE TABLE a (id INTEGER PRIMARY KEY)`}},
		{module: "second", name: "0001_idx", stmts: []string{`CREATE INDEX idx_a ON a(id)`}},
	}
	if err := applyMigrations(context.Background(), db, ms); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !tableExists(t, db, "a") {
		t.Error(`table "a" missing`)
	}

	// Prove the control is real: reversed, it must fail.
	rev := openTemp(t)
	if err := applyMigrations(context.Background(), rev, []migration{ms[1], ms[0]}); err == nil {
		t.Error("reversed migrations succeeded; this test cannot detect ordering")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	db := openTemp(t)
	// CREATE TABLE without IF NOT EXISTS: a second application that did not
	// skip would fail, so this proves the skip rather than assuming it.
	ms := []migration{{module: "only", name: "0001", stmts: []string{`CREATE TABLE a (id INTEGER PRIMARY KEY)`}}}
	for i := range 2 {
		if err := applyMigrations(context.Background(), db, ms); err != nil {
			t.Fatalf("apply %d: %v", i+1, err)
		}
	}
}

func TestAFailedMigrationLeavesNothingBehind(t *testing.T) {
	db := openTemp(t)
	ms := []migration{{module: "broken", name: "0001", stmts: []string{
		`CREATE TABLE good (id INTEGER PRIMARY KEY)`,
		`THIS IS NOT SQL`,
	}}}
	if err := applyMigrations(context.Background(), db, ms); err == nil {
		t.Fatal("expected an error from the invalid statement")
	}
	if tableExists(t, db, "good") {
		t.Error("the first statement survived a failed migration; it must roll back")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE id='broken/0001'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Error("a failed migration was recorded as applied")
	}
}

func TestAModuleCanEvolveAcrossMilestones(t *testing.T) {
	db := openTemp(t)
	first := []migration{{module: "planner", name: "0001_plan", stmts: []string{
		`CREATE TABLE plan (id INTEGER PRIMARY KEY)`}}}
	if err := applyMigrations(context.Background(), db, first); err != nil {
		t.Fatalf("first: %v", err)
	}
	// The same module adds a table a milestone later. Keying schema_migrations
	// on the module alone would skip this silently.
	second := append(first, migration{module: "planner", name: "0002_target", stmts: []string{
		`CREATE TABLE build_target (id INTEGER PRIMARY KEY)`}})
	if err := applyMigrations(context.Background(), db, second); err != nil {
		t.Fatalf("second: %v", err)
	}
	if !tableExists(t, db, "build_target") {
		t.Error("a module's later migration was skipped")
	}
}
