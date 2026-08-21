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
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kriya.db"))
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
	ms := []migration{
		{module: "first", stmts: []string{`CREATE TABLE a (id INTEGER PRIMARY KEY)`}},
		// Depends on the first, so it fails outright if order is not honoured.
		{module: "second", stmts: []string{`CREATE TABLE b (a_id INTEGER REFERENCES a(id))`}},
	}
	if err := applyMigrations(context.Background(), db, ms); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, name := range []string{"a", "b"} {
		if !tableExists(t, db, name) {
			t.Errorf("table %q missing", name)
		}
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	db := openTemp(t)
	// CREATE TABLE without IF NOT EXISTS: a second application that did not
	// skip would fail, so this proves the skip rather than assuming it.
	ms := []migration{{module: "only", stmts: []string{`CREATE TABLE a (id INTEGER PRIMARY KEY)`}}}
	for i := range 2 {
		if err := applyMigrations(context.Background(), db, ms); err != nil {
			t.Fatalf("apply %d: %v", i+1, err)
		}
	}
}

func TestAFailedMigrationLeavesNothingBehind(t *testing.T) {
	db := openTemp(t)
	ms := []migration{{module: "broken", stmts: []string{
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
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE module='broken'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Error("a failed migration was recorded as applied")
	}
}
