package planner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// HeadMigration is the durable head row.
//
// target_key is the PRIMARY KEY, and that uniqueness IS the bootstrap CAS: a
// target with no head yet has two concurrent first decompositions racing an
// INSERT, and exactly one can win. No separate "is there a head" read is
// needed, and no window exists between reading and inserting.
const HeadMigration = `
CREATE TABLE plan_head (
    target_key TEXT PRIMARY KEY,
    current    TEXT NOT NULL,
    generation INTEGER NOT NULL DEFAULT 0,
    fence      INTEGER NOT NULL DEFAULT 0
)`

// SQLHeads holds plan heads in SQLite.
type SQLHeads struct{ DB *sql.DB }

// Head reads a target's head row.
func (s SQLHeads) Head(ctx context.Context, targetKey string) (PlanHead, bool, error) {
	h := PlanHead{TargetKey: targetKey}
	err := s.DB.QueryRowContext(ctx,
		`SELECT current, generation, fence FROM plan_head WHERE target_key = ?`, targetKey).
		Scan(&h.Current, &h.Generation, &h.Fence)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanHead{}, false, nil
	}
	if err != nil {
		return PlanHead{}, false, fmt.Errorf("read plan head: %w", err)
	}
	return h, true, nil
}

// Replace performs the whole replacement in ONE transaction.
//
// Everything the spec calls atomic happens here together: the eligibility
// check, the predecessor's supersession, the head move, the generation bump
// and the fence raise. Split across transactions, a crash between any two
// would leave a head pointing at a plan that never superseded its predecessor
// — and pops admitted against a plan whose predecessor was never retired.
func (s SQLHeads) Replace(ctx context.Context, candidate Plan) (Replacement, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Replacement{}, fmt.Errorf("begin replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	verdict, err := replaceWithin(ctx, tx, candidate)
	if err != nil {
		return Replacement{}, err
	}
	if err := tx.Commit(); err != nil {
		return Replacement{}, fmt.Errorf("commit replacement: %w", err)
	}
	return verdict, nil
}

// replaceWithin is the replacement's body, inside the caller's transaction.
func replaceWithin(ctx context.Context, tx *sql.Tx, candidate Plan) (Replacement, error) {
	head, found, err := headWithin(ctx, tx, candidate.TargetKey)
	if err != nil {
		return Replacement{}, err
	}
	if !found {
		return bootstrapWithin(ctx, tx, candidate)
	}

	// The head's PLAN's intake generation, not the head row's own counter.
	// The two are different numbers and comparing the wrong one would let a
	// stale intake win against a head that had merely moved often.
	var headGeneration int
	var headState string
	err = tx.QueryRowContext(ctx,
		`SELECT generation, state FROM decomposition_plan WHERE decomposition_key = ?`,
		head.Current).Scan(&headGeneration, &headState)
	if errors.Is(err, sql.ErrNoRows) {
		return Replacement{}, fmt.Errorf("head of %s names plan %s, which does not exist",
			candidate.TargetKey, head.Current)
	}
	if err != nil {
		return Replacement{}, fmt.Errorf("read the head plan: %w", err)
	}

	// The head's plan must be ACTIVE. Replacing a head whose plan is itself
	// mid-replacement would interleave two supersessions over one predecessor.
	if headState != PlanActive || !Eligible(candidate.Generation, headGeneration) {
		return Replacement{Head: head, Landing: LandingFor(candidate.Generation, headGeneration)}, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE decomposition_plan SET state = ?, superseded_by = ?
		   WHERE decomposition_key = ? AND state = ?`,
		PlanSuperseded, candidate.Key, head.Current, PlanActive); err != nil {
		return Replacement{}, fmt.Errorf("supersede the predecessor: %w", err)
	}
	// The fence rises HERE, with the head move, and is lowered by a separate
	// activation once retirement has finished.
	if _, err := tx.ExecContext(ctx,
		`UPDATE plan_head SET current = ?, generation = generation + 1, fence = fence + 1
		   WHERE target_key = ? AND current = ?`,
		candidate.Key, candidate.TargetKey, head.Current); err != nil {
		return Replacement{}, fmt.Errorf("move the head: %w", err)
	}
	return Replacement{
		Won: true,
		Head: PlanHead{
			TargetKey: candidate.TargetKey, Current: candidate.Key,
			Generation: head.Generation + 1, Fence: head.Fence + 1,
		},
	}, nil
}

// bootstrapWithin installs the first head, the insert itself being the CAS.
func bootstrapWithin(ctx context.Context, tx *sql.Tx, candidate Plan) (Replacement, error) {
	// ON CONFLICT DO NOTHING, then read back: a concurrent first
	// decomposition that beat us leaves its own row, and the read tells us
	// whose it is. With no predecessor the retirement set is empty, so the
	// fence starts raised and the ordinary activation lowers it — one path,
	// not a special case that skips the fence.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO plan_head (target_key, current, generation, fence)
		 VALUES (?, ?, 1, 1) ON CONFLICT(target_key) DO NOTHING`,
		candidate.TargetKey, candidate.Key)
	if err != nil {
		return Replacement{}, fmt.Errorf("install the first head: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return Replacement{}, fmt.Errorf("install the first head: %w", err)
	}
	if rows == 1 {
		return Replacement{Won: true, Head: PlanHead{
			TargetKey: candidate.TargetKey, Current: candidate.Key, Generation: 1, Fence: 1,
		}}, nil
	}
	// Lost the insert race. Re-read and take the ordinary verdict.
	return replaceWithin(ctx, tx, candidate)
}

// headWithin reads the head row inside a transaction.
func headWithin(ctx context.Context, tx *sql.Tx, targetKey string) (PlanHead, bool, error) {
	h := PlanHead{TargetKey: targetKey}
	err := tx.QueryRowContext(ctx,
		`SELECT current, generation, fence FROM plan_head WHERE target_key = ?`, targetKey).
		Scan(&h.Current, &h.Generation, &h.Fence)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanHead{}, false, nil
	}
	if err != nil {
		return PlanHead{}, false, fmt.Errorf("read plan head: %w", err)
	}
	return h, true, nil
}

// Activate lowers the pop fence for a head that has finished retiring.
//
// Its OWN transaction. Lowering the fence in the head-moving transaction
// would admit pops in the window before retirement ran — against a plan whose
// predecessor still holds live tickets.
func (s SQLHeads) Activate(ctx context.Context, key string) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE plan_head SET fence = fence - 1 WHERE current = ? AND fence > 0`, key); err != nil {
		return fmt.Errorf("activate plan %s: %w", key, err)
	}
	return nil
}
