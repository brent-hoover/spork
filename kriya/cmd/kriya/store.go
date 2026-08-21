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
	stmts  []string
}

// applyMigrations applies each module's schema in the order given, once.
//
// Order is the caller's, not discovered: a module's tables may reference an
// earlier module's, and recovery reads them in a declared sequence. Applying
// is idempotent — a module already recorded in schema_migrations is skipped —
// so a crashed startup replays safely, which is the same write-ahead
// discipline every seam uses.
func applyMigrations(ctx context.Context, db *sql.DB, ms []migration) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (module TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, m := range ms {
		var seen int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM schema_migrations WHERE module = ?`, m.module).Scan(&seen); err != nil {
			return fmt.Errorf("check migration %s: %w", m.module, err)
		}
		if seen > 0 {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.module, err)
		}
		defer func() { _ = tx.Rollback() }()
		for i, stmt := range m.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s statement %d: %w", m.module, i, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (module) VALUES (?)`, m.module); err != nil {
			return fmt.Errorf("record migration %s: %w", m.module, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.module, err)
		}
	}
	return nil
}

// migrations is the declared order. Each module appends its own schema as it
// lands; it is empty until planner arrives in M2.
func migrations() []migration { return nil }
