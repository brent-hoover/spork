package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Migration is orchestrator's schema.
const Migration = `
CREATE TABLE build_run (
    id         TEXT PRIMARY KEY,
    ticket     TEXT NOT NULL,
    plan       TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL,
    gated_base TEXT NOT NULL DEFAULT '',
    error      TEXT NOT NULL DEFAULT '',
    attempt    INTEGER NOT NULL DEFAULT 0
)`

// RoundLimitMigration snapshots the pair loop's round limit onto the run.
//
// A separate migration for the same reason as every other second one: the
// ledger records which ran, and rewriting an applied one leaves existing
// databases claiming a column they do not have.
const RoundLimitMigration = `
ALTER TABLE build_run ADD COLUMN round_limit INTEGER NOT NULL DEFAULT 0`

// SQLStore stores build runs in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert writes a run.
func (s SQLStore) Upsert(ctx context.Context, r BuildRun) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO build_run (id, ticket, plan, state, gated_base, error, attempt, round_limit)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   ticket = excluded.ticket, plan = excluded.plan, state = excluded.state,
		   gated_base = excluded.gated_base, error = excluded.error,
		   attempt = excluded.attempt, round_limit = excluded.round_limit`,
		r.ID, r.Ticket, r.Plan, string(r.State), r.GatedBase, r.Error, r.Attempt,
		r.RoundLimit)
	if err != nil {
		return fmt.Errorf("upsert build run: %w", err)
	}
	return nil
}

// Find reads a run by id.
func (s SQLStore) Find(ctx context.Context, id string) (BuildRun, bool, error) {
	r := BuildRun{ID: id}
	var state string
	err := s.DB.QueryRowContext(ctx,
		`SELECT ticket, plan, state, gated_base, error, attempt, round_limit
		   FROM build_run WHERE id = ?`, id).
		Scan(&r.Ticket, &r.Plan, &state, &r.GatedBase, &r.Error, &r.Attempt, &r.RoundLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return BuildRun{}, false, nil
	}
	if err != nil {
		return BuildRun{}, false, fmt.Errorf("read build run: %w", err)
	}
	r.State = State(state)
	return r, true, nil
}
