package planner

import (
	"context"
	"database/sql"
	"fmt"
)

// SQLSteps persists plan mutation sequences in SQLite.
type SQLSteps struct{ DB *sql.DB }

// Write persists a whole sequence in ONE transaction.
//
// All or nothing, because a partially written sequence is one a recovery
// replays incompletely — and it cannot tell the difference between "the plan
// had four steps" and "the plan had eight and four were lost", so it would
// report a plan whole with half its mutations never made.
func (s SQLSteps) Write(ctx context.Context, steps []Step) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sequence write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, step := range steps {
		// DO NOTHING on conflict: the sequence is derived deterministically
		// from the plan, so a re-derivation is identical and overwriting
		// would reset the durable state of steps already issued.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plan_step (plan, seq, ordinal, other, kind, key, state, issue)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(plan, seq) DO NOTHING`,
			step.Plan, step.Seq, step.Ordinal, step.Other,
			step.Kind, step.Key, step.State, step.Issue); err != nil {
			return fmt.Errorf("write step %d: %w", step.Seq, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sequence: %w", err)
	}
	return nil
}

// ForPlan reads a plan's sequence in seq order.
func (s SQLSteps) ForPlan(ctx context.Context, plan string) ([]Step, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT plan, seq, ordinal, other, kind, key, state, issue
		   FROM plan_step WHERE plan = ? ORDER BY seq`, plan)
	if err != nil {
		return nil, fmt.Errorf("query sequence: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Step
	for rows.Next() {
		var step Step
		if err := rows.Scan(&step.Plan, &step.Seq, &step.Ordinal, &step.Other,
			&step.Kind, &step.Key, &step.State, &step.Issue); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		out = append(out, step)
	}
	// Checked: a cursor failing mid-iteration otherwise returns a SHORT
	// sequence, which reads as a plan with fewer mutations than it has.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sequence: %w", err)
	}
	return out, nil
}

// Mark moves one step's durable state.
//
// The issue id is only ever written, never blanked: a create fills it and the
// completion mark carries the same value back. Blanking it on a later mark
// would leave a completed create whose result nothing can find.
func (s SQLSteps) Mark(ctx context.Context, plan string, seq int, state, issue string) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE plan_step SET state = ?, issue = CASE WHEN ? = '' THEN issue ELSE ? END
		   WHERE plan = ? AND seq = ?`,
		state, issue, issue, plan, seq); err != nil {
		return fmt.Errorf("mark step %d: %w", seq, err)
	}
	return nil
}
