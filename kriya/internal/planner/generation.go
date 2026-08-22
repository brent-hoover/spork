package planner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Attempt states.
const (
	AttemptPending  = "pending"
	AttemptComplete = "complete"
)

// IntakeAttempt records one intake under its idempotency token.
//
// The token is the caller's; the generation is kriya's. Recording the attempt
// BEFORE the plan is created is what makes a crash in that window replayable:
// the retry finds the recorded attempt and reuses its generation rather than
// allocating a second and superseding itself.
type IntakeAttempt struct {
	Token      string
	TargetKey  string
	Generation int
	SpecHash   string
	State      string
}

// SpecMapping is the newest intake a project has seen.
//
// Unplanned binds read it, so it must never regress: a delayed plan seed
// carrying an older generation arriving after a newer intake would otherwise
// point every later bind at a superseded snapshot.
type SpecMapping struct {
	TargetKey    string
	Generation   int
	SnapshotHash string
}

// AttemptStore allocates generations and records attempts.
type AttemptStore interface {
	// Reserve returns the attempt for token, allocating the next generation
	// for the target the first time that token is seen.
	Reserve(ctx context.Context, token, targetKey string) (IntakeAttempt, error)
	// Complete stamps an attempt with the snapshot it pinned.
	Complete(ctx context.Context, token, specHash string) error
	// MapSpec upserts the mapping, keeping the HIGHER generation.
	MapSpec(ctx context.Context, m SpecMapping) error
	// Mapping reads a target's current mapping.
	Mapping(ctx context.Context, targetKey string) (SpecMapping, bool, error)
}

// AttemptMigration is planner's third table set.
const AttemptMigration = `
CREATE TABLE intake_attempt (
    token      TEXT PRIMARY KEY,
    target_key TEXT NOT NULL,
    generation INTEGER NOT NULL,
    spec_hash  TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL
);
CREATE TABLE target_generation (
    target_key TEXT PRIMARY KEY,
    current    INTEGER NOT NULL
);
CREATE TABLE spec_mapping (
    target_key    TEXT PRIMARY KEY,
    generation    INTEGER NOT NULL,
    snapshot_hash TEXT NOT NULL
)`

// SQLAttempts stores intake attempts in SQLite.
type SQLAttempts struct{ DB *sql.DB }

// Reserve allocates the next generation, or resumes a recorded attempt.
//
// The allocation is a compare-and-swap inside one transaction, so two
// differently keyed intakes racing for the same target serialize: one reserves
// N, the loser re-reads and reserves N+1. Two attempts never hold the same
// generation, which is what keeps supersession ordering total.
func (s SQLAttempts) Reserve(ctx context.Context, token, targetKey string) (IntakeAttempt, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return IntakeAttempt{}, fmt.Errorf("begin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, found, err := readAttempt(ctx, tx, token)
	if err != nil {
		return IntakeAttempt{}, err
	}
	if found {
		// The SAME token is the same intake. Reusing its generation is what
		// makes a crash before the plan replayable without a duplicate
		// supersession.
		return existing, nil
	}

	generation, err := nextGeneration(ctx, tx, targetKey)
	if err != nil {
		return IntakeAttempt{}, err
	}
	attempt := IntakeAttempt{
		Token: token, TargetKey: targetKey,
		Generation: generation, State: AttemptPending,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO intake_attempt (token, target_key, generation, state) VALUES (?, ?, ?, ?)`,
		attempt.Token, attempt.TargetKey, attempt.Generation, attempt.State); err != nil {
		return IntakeAttempt{}, fmt.Errorf("record attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return IntakeAttempt{}, fmt.Errorf("commit reserve: %w", err)
	}
	return attempt, nil
}

func readAttempt(ctx context.Context, tx *sql.Tx, token string) (IntakeAttempt, bool, error) {
	a := IntakeAttempt{Token: token}
	err := tx.QueryRowContext(ctx,
		`SELECT target_key, generation, spec_hash, state FROM intake_attempt WHERE token = ?`, token).
		Scan(&a.TargetKey, &a.Generation, &a.SpecHash, &a.State)
	if errors.Is(err, sql.ErrNoRows) {
		// Absence is not failure: a token never seen is the ordinary case.
		return IntakeAttempt{}, false, nil
	}
	if err != nil {
		return IntakeAttempt{}, false, fmt.Errorf("read attempt: %w", err)
	}
	return a, true, nil
}

// nextGeneration increments the target's counter and returns the new value.
func nextGeneration(ctx context.Context, tx *sql.Tx, targetKey string) (int, error) {
	var current int
	err := tx.QueryRowContext(ctx,
		`SELECT current FROM target_generation WHERE target_key = ?`, targetKey).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		current = 0
	case err != nil:
		return 0, fmt.Errorf("read generation: %w", err)
	}
	next := current + 1
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO target_generation (target_key, current) VALUES (?, ?)
		 ON CONFLICT(target_key) DO UPDATE SET current = excluded.current`,
		targetKey, next); err != nil {
		return 0, fmt.Errorf("advance generation: %w", err)
	}
	return next, nil
}

// Complete stamps an attempt with the snapshot it pinned.
func (s SQLAttempts) Complete(ctx context.Context, token, specHash string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE intake_attempt SET spec_hash = ?, state = ? WHERE token = ?`,
		specHash, AttemptComplete, token)
	if err != nil {
		return fmt.Errorf("complete attempt: %w", err)
	}
	return nil
}

// MapSpec upserts the mapping, keeping the higher generation.
//
// The fence is in the WHERE clause, not in the caller: a delayed plan seed
// carrying an older generation loses the upsert wherever it arrives from, and
// ordering that in application code would only move the race.
func (s SQLAttempts) MapSpec(ctx context.Context, m SpecMapping) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO spec_mapping (target_key, generation, snapshot_hash) VALUES (?, ?, ?)
		 ON CONFLICT(target_key) DO UPDATE SET
		   generation = excluded.generation,
		   snapshot_hash = excluded.snapshot_hash
		 WHERE excluded.generation > spec_mapping.generation`,
		m.TargetKey, m.Generation, m.SnapshotHash)
	if err != nil {
		return fmt.Errorf("map spec: %w", err)
	}
	return nil
}

// Mapping reads a target's current mapping.
func (s SQLAttempts) Mapping(ctx context.Context, targetKey string) (SpecMapping, bool, error) {
	m := SpecMapping{TargetKey: targetKey}
	err := s.DB.QueryRowContext(ctx,
		`SELECT generation, snapshot_hash FROM spec_mapping WHERE target_key = ?`, targetKey).
		Scan(&m.Generation, &m.SnapshotHash)
	if errors.Is(err, sql.ErrNoRows) {
		return SpecMapping{}, false, nil
	}
	if err != nil {
		return SpecMapping{}, false, fmt.Errorf("read mapping: %w", err)
	}
	return m, true, nil
}
