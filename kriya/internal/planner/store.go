package planner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Migration is planner's schema. The composition root sequences it with the
// other modules'; each module owns its own tables.
const Migration = `
CREATE TABLE spec_snapshot (
    hash              TEXT PRIMARY KEY,
    content           TEXT NOT NULL,
    resolved_commands TEXT NOT NULL,
    created           TEXT NOT NULL
)`

// TargetMigration is planner's second table. Separate from the first because
// a module's schema evolves across milestones and each migration is recorded
// by its own id; folding it into the first would never run on a database that
// already applied it.
const TargetMigration = `
CREATE TABLE build_target (
    target_key TEXT PRIMARY KEY,
    spec_hash  TEXT NOT NULL,
    project_id TEXT NOT NULL DEFAULT '',
    epic_id    TEXT NOT NULL DEFAULT '',
    epic_state TEXT NOT NULL
)`

// SQLSnapshots stores snapshots in SQLite.
type SQLSnapshots struct{ DB *sql.DB }

// Put stores a snapshot. Pinning the same content twice is not an error:
// snapshots are content-addressed, so an identical hash is the same snapshot,
// and a re-intake of unchanged files must not fail.
func (s SQLSnapshots) Put(ctx context.Context, snap Snapshot) error {
	content, err := json.Marshal(snap.Content)
	if err != nil {
		return fmt.Errorf("marshal content: %w", err)
	}
	commands, err := json.Marshal(snap.ResolvedCommands)
	if err != nil {
		return fmt.Errorf("marshal resolved commands: %w", err)
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO spec_snapshot (hash, content, resolved_commands, created)
		 VALUES (?, ?, ?, ?) ON CONFLICT(hash) DO NOTHING`,
		snap.Hash, string(content), string(commands), snap.Created.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return nil
}

// Get reads a snapshot by hash.
func (s SQLSnapshots) Get(ctx context.Context, hash string) (Snapshot, error) {
	var content, commands, created string
	err := s.DB.QueryRowContext(ctx,
		`SELECT content, resolved_commands, created FROM spec_snapshot WHERE hash = ?`, hash).
		Scan(&content, &commands, &created)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot %s: %w", hash, err)
	}
	snap := Snapshot{Hash: hash}
	if err := json.Unmarshal([]byte(content), &snap.Content); err != nil {
		return Snapshot{}, fmt.Errorf("unmarshal content: %w", err)
	}
	if err := json.Unmarshal([]byte(commands), &snap.ResolvedCommands); err != nil {
		return Snapshot{}, fmt.Errorf("unmarshal resolved commands: %w", err)
	}
	if snap.Created, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Snapshot{}, fmt.Errorf("parse created: %w", err)
	}
	return snap, nil
}

// Count reports how many snapshots are pinned. It exists for the acceptance
// assertion that a refused intake pins nothing.
func (s SQLSnapshots) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM spec_snapshot`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count snapshots: %w", err)
	}
	return n, nil
}

// SQLTargets stores build targets in SQLite.
type SQLTargets struct{ DB *sql.DB }

// Upsert writes a target, replacing any row with the same key.
func (s SQLTargets) Upsert(ctx context.Context, t BuildTarget) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO build_target (target_key, spec_hash, project_id, epic_id, epic_state)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(target_key) DO UPDATE SET
		   spec_hash = excluded.spec_hash,
		   project_id = excluded.project_id,
		   epic_id = excluded.epic_id,
		   epic_state = excluded.epic_state`,
		t.TargetKey, t.SpecHash, t.ProjectID, t.EpicID, t.EpicState)
	if err != nil {
		return fmt.Errorf("upsert build target: %w", err)
	}
	return nil
}

// Find reads a target by key.
func (s SQLTargets) Find(ctx context.Context, targetKey string) (BuildTarget, bool, error) {
	t := BuildTarget{TargetKey: targetKey}
	err := s.DB.QueryRowContext(ctx,
		`SELECT spec_hash, project_id, epic_id, epic_state FROM build_target WHERE target_key = ?`,
		targetKey).Scan(&t.SpecHash, &t.ProjectID, &t.EpicID, &t.EpicState)
	if errors.Is(err, sql.ErrNoRows) {
		// Absence is not failure: the first intake of a target has no row.
		return BuildTarget{}, false, nil
	}
	if err != nil {
		return BuildTarget{}, false, fmt.Errorf("read build target: %w", err)
	}
	return t, true, nil
}
