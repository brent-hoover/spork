package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"kriya/internal/agent"
	"kriya/internal/architect"
	kctx "kriya/internal/context"
	"kriya/internal/devloop"
	"kriya/internal/gates"
	"kriya/internal/orchestrator"
	"kriya/internal/owner"
	"kriya/internal/planner"
	"kriya/internal/reviewbridge"
	"kriya/internal/workspace"
)

// migration is one module's schema, applied as a unit.
//
// Each module owns its own tables — there is no shared store package, which
// is sutra's shape and what keeps the arch-go allowlists honest. The
// composition root is the only thing that sees them all, so it is the only
// thing that can sequence them.
type migration struct {
	module string
	// name is unique within the module. Keying only on module would freeze a
	// module's schema after its first migration, and modules DO evolve across
	// milestones by design — planner splits M2/M4 and orchestrator M3/M4/M5 —
	// so a database created at M2 would silently skip every later table.
	name  string
	stmts []string
}

// id is the key recorded in schema_migrations.
func (m migration) id() string { return m.module + "/" + m.name }

// ensureMigrationLedger creates the table recording which migrations have run.
//
// There is deliberately NO conversion of an older shape. An earlier version of
// this file keyed the table on `module` rather than `id`, and a review asked
// for in-place conversion — but no such database has ever existed: both
// shapes lived and died inside this unreleased branch. The conversion was
// also wrong, renaming the column while leaving values like "planner" that no
// longer match ids like "planner/0001_plan", so the first migration would
// rerun and fail on its existing tables. Speculative compatibility for a
// database with no instances, carrying a bug of its own.
func ensureMigrationLedger(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (id TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

// applied reports whether a migration has already run.
func applied(ctx context.Context, db *sql.DB, m migration) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE id = ?`, m.id()).Scan(&n); err != nil {
		return false, fmt.Errorf("check migration %s: %w", m.id(), err)
	}
	return n > 0, nil
}

// applyOne runs a migration and records it, atomically. A failure leaves
// neither its statements nor its ledger row behind.
func applyOne(ctx context.Context, db *sql.DB, m migration) (err error) {
	tx, txErr := db.BeginTx(ctx, nil)
	if txErr != nil {
		return fmt.Errorf("begin migration %s: %w", m.id(), txErr)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	for i, stmt := range m.stmts {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration %s statement %d: %w", m.id(), i, err)
		}
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (id) VALUES (?)`, m.id()); err != nil {
		return fmt.Errorf("record migration %s: %w", m.id(), err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.id(), err)
	}
	return nil
}

// applyMigrations applies each module's schema in the order given, once.
//
// Order is the caller's, not discovered: a module's tables may reference an
// earlier module's, and recovery reads them in a declared sequence. Applying
// is idempotent — a migration already recorded is skipped — so a crashed
// startup replays safely, which is the same write-ahead discipline every seam
// uses.
func applyMigrations(ctx context.Context, db *sql.DB, ms []migration) error {
	if err := ensureMigrationLedger(ctx, db); err != nil {
		return err
	}
	for _, m := range ms {
		done, err := applied(ctx, db, m)
		if err != nil {
			return err
		}
		if done {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// migrations is the declared order. Each module appends its own schema as it
// lands, and order is the caller's because a module's tables may reference an
// earlier module's.
func migrations() []migration {
	return []migration{
		{module: "planner", name: "0001_spec_snapshot", stmts: []string{planner.Migration}},
		{module: "planner", name: "0002_build_target", stmts: []string{planner.TargetMigration}},
		{module: "planner", name: "0003_intake_generation", stmts: splitSQL(planner.AttemptMigration)},
		{module: "planner", name: "0004_snapshot_law", stmts: splitSQL(planner.LawMigration)},
		{module: "agent", name: "0001_invocation", stmts: []string{agent.Migration}},
		{module: "workspace", name: "0001_workspace", stmts: []string{workspace.Migration}},
		{module: "workspace", name: "0002_removal", stmts: splitSQL(workspace.RemovalMigration)},
		{module: "orchestrator", name: "0001_build_run", stmts: []string{orchestrator.Migration}},
		{module: "orchestrator", name: "0002_round_limit", stmts: splitSQL(orchestrator.RoundLimitMigration)},
		{module: "orchestrator", name: "0003_review_submission", stmts: splitSQL(orchestrator.SubmissionMigration)},
		{module: "architect", name: "0001_intervention", stmts: splitSQL(architect.Migration)},
		{module: "owner", name: "0001_validation", stmts: splitSQL(owner.Migration)},
		{module: "gates", name: "0001_gate_result", stmts: []string{gates.Migration}},
		{module: "devloop", name: "0001_dev_session", stmts: []string{devloop.Migration}},
		{module: "devloop", name: "0002_dev_session_rounds", stmts: splitSQL(devloop.RoundsMigration)},
		{module: "devloop", name: "0003_thread_capture", stmts: splitSQL(devloop.CaptureMigration)},
		{module: "reviewbridge", name: "0001_review_round", stmts: splitSQL(reviewbridge.Migration)},
		{module: "reviewbridge", name: "0002_response_lifecycle", stmts: splitSQL(reviewbridge.ResponseMigration)},
		{module: "context", name: "0001_context_and_learnings", stmts: splitSQL(kctx.Migration)},
	}
}

// splitSQL breaks a multi-statement migration into single statements.
//
// database/sql executes ONE statement per Exec with most drivers, so a
// migration written as several would silently apply only the first — creating
// one table and leaving the rest missing until something queried them.
func splitSQL(migration string) []string {
	var out []string
	for _, part := range strings.Split(migration, ";") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
