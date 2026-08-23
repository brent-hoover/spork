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

// SubmissionMigration adds the review submission's write-ahead columns.
const SubmissionMigration = `
ALTER TABLE build_run ADD COLUMN review_key TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN review_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN review_session TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN review_state TEXT NOT NULL DEFAULT 'none';
ALTER TABLE build_run ADD COLUMN review_id TEXT NOT NULL DEFAULT ''`

// runColumns is every column a BuildRun reads back, in scan order.
const runColumns = `ticket, plan, state, gated_base, error, attempt, round_limit,
	review_key, review_commit, review_session, review_state, review_id`

// SQLStore stores build runs in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert writes a run.
func (s SQLStore) Upsert(ctx context.Context, r BuildRun) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO build_run (id, ticket, plan, state, gated_base, error, attempt,
		   round_limit, review_key, review_commit, review_session, review_state, review_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   ticket = excluded.ticket, plan = excluded.plan, state = excluded.state,
		   gated_base = excluded.gated_base, error = excluded.error,
		   attempt = excluded.attempt, round_limit = excluded.round_limit,
		   review_key = excluded.review_key, review_commit = excluded.review_commit,
		   review_session = excluded.review_session, review_state = excluded.review_state,
		   review_id = excluded.review_id`,
		r.ID, r.Ticket, r.Plan, string(r.State), r.GatedBase, r.Error, r.Attempt,
		r.RoundLimit, r.ReviewKey, r.ReviewCommit, r.ReviewSession,
		reviewStateOf(r), r.ReviewID)
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
		`SELECT `+runColumns+` FROM build_run WHERE id = ?`, id).
		Scan(&r.Ticket, &r.Plan, &state, &r.GatedBase, &r.Error, &r.Attempt, &r.RoundLimit,
			&r.ReviewKey, &r.ReviewCommit, &r.ReviewSession, &r.ReviewState, &r.ReviewID)
	if errors.Is(err, sql.ErrNoRows) {
		return BuildRun{}, false, nil
	}
	if err != nil {
		return BuildRun{}, false, fmt.Errorf("read build run: %w", err)
	}
	r.State = State(state)
	return r, true, nil
}

// reviewStateOf defaults an unset state.
//
// A zero-valued BuildRun has no review state, and writing "" would put a value
// outside the declared enum in the column.
func reviewStateOf(r BuildRun) string {
	if r.ReviewState == "" {
		return SubmitNone
	}
	return r.ReviewState
}

// Submitting lists runs whose review submission a crash left in flight.
func (s SQLStore) Submitting(ctx context.Context) ([]BuildRun, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, `+runColumns+` FROM build_run WHERE review_state = ? ORDER BY id`,
		SubmitSubmitting)
	if err != nil {
		return nil, fmt.Errorf("query submitting runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []BuildRun
	for rows.Next() {
		var r BuildRun
		var state string
		if err := rows.Scan(&r.ID, &r.Ticket, &r.Plan, &state, &r.GatedBase, &r.Error,
			&r.Attempt, &r.RoundLimit, &r.ReviewKey, &r.ReviewCommit,
			&r.ReviewSession, &r.ReviewState, &r.ReviewID); err != nil {
			return nil, fmt.Errorf("scan submitting run: %w", err)
		}
		r.State = State(state)
		out = append(out, r)
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list that reads exactly like "every review reached sutra".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate submitting runs: %w", err)
	}
	return out, nil
}
