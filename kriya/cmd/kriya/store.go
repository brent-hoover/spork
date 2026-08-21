package main

import (
	"context"
	"database/sql"
	"fmt"
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

// applyMigrations applies each module's schema in the order given, once.
//
// Order is the caller's, not discovered: a module's tables may reference an
// earlier module's, and recovery reads them in a declared sequence. Applying
// is idempotent — a module already recorded in schema_migrations is skipped —
// so a crashed startup replays safely, which is the same write-ahead
// discipline every seam uses.
func applyMigrations(ctx context.Context, db *sql.DB, ms []migration) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (id TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	// An earlier build keyed this table on `module` rather than `id`, and
	// CREATE TABLE IF NOT EXISTS does not convert an existing schema — the
	// first query for `id` would fail and block startup. Convert in place.
	// Cheap to carry and impossible to notice if it is missing.
	var legacy int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('schema_migrations') WHERE name = 'module'`).
		Scan(&legacy); err != nil {
		return fmt.Errorf("inspect schema_migrations: %w", err)
	}
	if legacy > 0 {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE schema_migrations RENAME COLUMN module TO id`); err != nil {
			return fmt.Errorf("migrate schema_migrations to id: %w", err)
		}
	}
	for _, m := range ms {
		var seen int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM schema_migrations WHERE id = ?`, m.id()).Scan(&seen); err != nil {
			return fmt.Errorf("check migration %s: %w", m.id(), err)
		}
		if seen > 0 {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.id(), err)
		}
		defer func() { _ = tx.Rollback() }()
		for i, stmt := range m.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s statement %d: %w", m.id(), i, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (id) VALUES (?)`, m.id()); err != nil {
			return fmt.Errorf("record migration %s: %w", m.id(), err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.id(), err)
		}
	}
	return nil
}

// migrations is the declared order. Each module appends its own schema as it
// lands; it is empty until planner arrives in M2.
func migrations() []migration { return nil }
