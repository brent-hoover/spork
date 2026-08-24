package planner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EpochMigration is the completion epoch and its advance log.
//
// TWO tables, because they answer different questions: the log is insert-once
// evidence of WHY the epoch moved, and the counter is the fence a claim is
// stamped against. They move together in one transaction — an advance that
// logged without counting would let a spent claim stamp.
const EpochMigration = `
CREATE TABLE completion_advance (
    advance_key  TEXT PRIMARY KEY,
    target_key   TEXT NOT NULL,
    cause        TEXT NOT NULL,
    event        TEXT NOT NULL DEFAULT '',
    local_source TEXT NOT NULL DEFAULT ''
);
CREATE TABLE completion_state (
    target_key      TEXT PRIMARY KEY,
    completion_epoch INTEGER NOT NULL DEFAULT 0,
    stamped          INTEGER NOT NULL DEFAULT 0,
    stamped_epoch    INTEGER NOT NULL DEFAULT 0
)`

// SQLAdvances persists completion advances in SQLite.
type SQLAdvances struct{ DB *sql.DB }

// Insert records an advance and moves the epoch, atomically.
//
// One transaction, because the log and the counter are two halves of one fact.
// An advance logged without counting leaves a spent claim able to stamp; a
// count without a log leaves recovery unable to tell what it must reconcile.
func (s SQLAdvances) Insert(ctx context.Context, a CompletionAdvance) (fresh bool, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin advance: %w", err)
	}
	defer func() {
		if err != nil || !fresh {
			_ = tx.Rollback()
		}
	}()

	// DO NOTHING rather than a read-then-write: two consumers of the same
	// event would both find nothing and both insert, and the second would
	// fail on the key anyway — so the key is asked to decide, once.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO completion_advance (advance_key, target_key, cause, event, local_source)
		 VALUES (?, ?, ?, ?, ?) ON CONFLICT(advance_key) DO NOTHING`,
		a.Key, a.TargetKey, a.Cause, a.Event, a.LocalSource)
	if err != nil {
		return false, fmt.Errorf("insert advance: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert advance: %w", err)
	}
	if n == 0 {
		// Replayed. The epoch does not move, and the recorded cause stands.
		return false, nil
	}

	// The stamp clears in the same statement that moves the counter: a target
	// whose work came back is not complete, whatever it said a moment ago.
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO completion_state (target_key, completion_epoch, stamped, stamped_epoch)
		 VALUES (?, 1, 0, 0)
		 ON CONFLICT(target_key) DO UPDATE SET
		   completion_epoch = completion_state.completion_epoch + 1,
		   stamped = 0, stamped_epoch = 0`, a.TargetKey); err != nil {
		return false, fmt.Errorf("advance completion epoch: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("commit advance: %w", err)
	}
	return true, nil
}

// Epoch reads a target's completion epoch.
func (s SQLAdvances) Epoch(ctx context.Context, targetKey string) (int, error) {
	var epoch int
	err := s.DB.QueryRowContext(ctx,
		`SELECT completion_epoch FROM completion_state WHERE target_key = ?`, targetKey).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing has ever advanced it. Zero is the honest answer, not an
		// error: a target whose work never came back is at its first epoch.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read completion epoch: %w", err)
	}
	return epoch, nil
}

// Stamp records completion while the epoch still equals the claim's.
//
// The WHERE clause is the compare-and-swap. Reading the epoch and then writing
// would leave a window in which an advance lands between them, and the stamp
// would record a completion the advance had just invalidated.
func (s SQLAdvances) Stamp(ctx context.Context, targetKey string, claimEpoch int) (bool, error) {
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO completion_state (target_key, completion_epoch, stamped, stamped_epoch)
		 SELECT ?, ?, 1, ?
		 WHERE NOT EXISTS (SELECT 1 FROM completion_state WHERE target_key = ?)
		   AND ? = 0`,
		targetKey, claimEpoch, claimEpoch, targetKey, claimEpoch)
	if err != nil {
		return false, fmt.Errorf("stamp completion: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	res, err = s.DB.ExecContext(ctx,
		`UPDATE completion_state SET stamped = 1, stamped_epoch = ?
		   WHERE target_key = ? AND completion_epoch = ?`,
		claimEpoch, targetKey, claimEpoch)
	if err != nil {
		return false, fmt.Errorf("stamp completion: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("stamp completion: %w", err)
	}
	return n > 0, nil
}

// Stamped reports whether a target is stamped complete, and at which epoch.
func (s SQLAdvances) Stamped(ctx context.Context, targetKey string) (bool, int, error) {
	var stamped, epoch int
	err := s.DB.QueryRowContext(ctx,
		`SELECT stamped, stamped_epoch FROM completion_state WHERE target_key = ?`,
		targetKey).Scan(&stamped, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("read completion stamp: %w", err)
	}
	return stamped == 1, epoch, nil
}
