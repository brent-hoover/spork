package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openTemp(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "kriya.db")))
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

func TestForeignKeysAreActuallyEnforced(t *testing.T) {
	// Opened through the production dsn(), so this tests the real connection
	// string rather than a copy of its pragmas.
	db := openTemp(t)
	for _, stmt := range []string{
		`CREATE TABLE parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE child (parent_id INTEGER REFERENCES parent(id))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO child (parent_id) VALUES (999)`); err == nil {
		t.Fatal("inserted a child row with no parent; foreign keys are not enforced")
	}
}

// TestAShippedMigrationIsNeverEdited pins every registered migration's SQL.
//
// A migration that has run is never run again: the ledger records its id, and
// applyMigrations skips it. So editing one in place changes what a FRESH
// database gets and nothing at all for an existing one — the two diverge
// silently, and every query naming the new column fails against the old
// database. That is exactly how merge_attempt.resource was lost.
//
// When this test fails, the fix is a NEW migration that alters the table, not
// a new fingerprint. Update a fingerprint only for a migration that has never
// been released.
const migrationFingerprints = `
agent/0001_invocation 38098861a704eef4
planner/0001_spec_snapshot 2e71925f1fb4b9ad
architect/0001_intervention be3362a7c5d1c285
context/0001_context_and_learnings bd3bd3d13bc91ec0
context/0002_learning_provenance 1f18c63a80d00a65
devloop/0001_dev_session c79edf04cf3376f1
devloop/0002_dev_session_rounds 23309c1fa0a8e2a3
devloop/0004_session_system_file 27d939451710f4d2
devloop/0003_thread_capture b57ee76136258e3f
gates/0001_gate_result d6e82772accb1bd2
orchestrator/0001_build_run 8e5c7bff199b7bb0
orchestrator/0002_round_limit 9d814ca7c6871347
orchestrator/0003_review_submission 466873c4bd13b8c7
orchestrator/0004_merge_attempt 81ba2052d2a347d0
orchestrator/0005_completion 80f9a5c131d5c26b
orchestrator/0006_pop_ordinal 37fef0da3df58359
orchestrator/0007_feed_cursor 96616e59eec48054
orchestrator/0008_branch_head 103dcf0755736d53
orchestrator/0009_issue_and_branch 0a433f0bae1f6633
orchestrator/0010_merge_resource 14dee5d948d188fa
owner/0001_validation c7f67856bc940d53
planner/0002_build_target daddc0ce933b30e3
planner/0003_intake_generation a7061af573e343ac
planner/0004_snapshot_law c03d5f878c37cf1d
planner/0005_planned_ticket 315c799e0c3ee7fa
reviewbridge/0001_review_round 2965b318002b15e8
reviewbridge/0002_response_lifecycle 3d842b605063f8e8
workspace/0001_workspace 4aca62f3b6fc357c
workspace/0002_removal ed2a72ca5aecaf13
`

func TestAShippedMigrationIsNeverEdited(t *testing.T) {
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(migrationFingerprints), "\n") {
		id, sum, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			t.Fatalf("malformed fingerprint line %q", line)
		}
		want[id] = sum
	}
	got := map[string]string{}
	for _, m := range migrations() {
		sum := sha256.Sum256([]byte(strings.Join(m.stmts, ";")))
		got[m.id()] = hex.EncodeToString(sum[:])[:16]
	}
	for id, sum := range got {
		switch recorded, known := want[id]; {
		case !known:
			t.Errorf("migration %s has no fingerprint; add:\n%s %s", id, id, sum)
		case recorded != sum:
			t.Errorf("migration %s was EDITED after it shipped.\n"+
				"A database that already ran it will never see the change.\n"+
				"Add a new migration that alters the table instead.", id)
		}
	}
	for id := range want {
		if _, still := got[id]; !still {
			t.Errorf("migration %s was removed; a database that ran it cannot un-run it", id)
		}
	}
}
