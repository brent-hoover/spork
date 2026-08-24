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

// LawMigration pins each module's boundary and the project's constitution
// alongside the files.
//
// A second migration rather than an edit to the first: the ledger records
// which migrations ran, and rewriting an applied one leaves every existing
// database claiming to have columns it does not have.
const LawMigration = `
ALTER TABLE spec_snapshot ADD COLUMN law TEXT NOT NULL DEFAULT '[]';
ALTER TABLE spec_snapshot ADD COLUMN constitution TEXT NOT NULL DEFAULT '[]'`

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
    epic_state TEXT NOT NULL,
    project_key TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL DEFAULT ''
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
	law, err := json.Marshal(snap.Law)
	if err != nil {
		return fmt.Errorf("marshal law: %w", err)
	}
	constitution, err := json.Marshal(snap.Constitution)
	if err != nil {
		return fmt.Errorf("marshal constitution: %w", err)
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO spec_snapshot (hash, content, resolved_commands, law, constitution, created)
		 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(hash) DO NOTHING`,
		snap.Hash, string(content), string(commands), string(law), string(constitution),
		snap.Created.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return nil
}

// Get reads a snapshot by hash.
func (s SQLSnapshots) Get(ctx context.Context, hash string) (Snapshot, error) {
	var content, commands, law, constitution, created string
	err := s.DB.QueryRowContext(ctx,
		`SELECT content, resolved_commands, law, constitution, created
		   FROM spec_snapshot WHERE hash = ?`, hash).
		Scan(&content, &commands, &law, &constitution, &created)
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
	if err := json.Unmarshal([]byte(law), &snap.Law); err != nil {
		return Snapshot{}, fmt.Errorf("unmarshal law: %w", err)
	}
	if err := json.Unmarshal([]byte(constitution), &snap.Constitution); err != nil {
		return Snapshot{}, fmt.Errorf("unmarshal constitution: %w", err)
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
		`INSERT INTO build_target
		   (target_key, spec_hash, project_id, epic_id, epic_state, project_key, name, actor)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(target_key) DO UPDATE SET
		   spec_hash = excluded.spec_hash,
		   project_id = excluded.project_id,
		   epic_id = excluded.epic_id,
		   epic_state = excluded.epic_state,
		   project_key = excluded.project_key,
		   name = excluded.name,
		   actor = excluded.actor`,
		t.TargetKey, t.SpecHash, t.ProjectID, t.EpicID, t.EpicState,
		t.ProjectKey, t.Name, t.Actor)
	if err != nil {
		return fmt.Errorf("upsert build target: %w", err)
	}
	return nil
}

// Find reads a target by key.
func (s SQLTargets) Find(ctx context.Context, targetKey string) (BuildTarget, bool, error) {
	t := BuildTarget{TargetKey: targetKey}
	err := s.DB.QueryRowContext(ctx,
		`SELECT spec_hash, project_id, epic_id, epic_state, project_key, name, actor
		 FROM build_target WHERE target_key = ?`,
		targetKey).Scan(&t.SpecHash, &t.ProjectID, &t.EpicID, &t.EpicState,
		&t.ProjectKey, &t.Name, &t.Actor)
	if errors.Is(err, sql.ErrNoRows) {
		// Absence is not failure: the first intake of a target has no row.
		return BuildTarget{}, false, nil
	}
	if err != nil {
		return BuildTarget{}, false, fmt.Errorf("read build target: %w", err)
	}
	return t, true, nil
}

// Pending lists targets a crash left mid-creation.
func (s SQLTargets) Pending(ctx context.Context) ([]BuildTarget, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT target_key, spec_hash, project_id, epic_id, epic_state, project_key, name, actor
		 FROM build_target WHERE epic_state = ? ORDER BY target_key`, EpicPending)
	if err != nil {
		return nil, fmt.Errorf("query pending targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []BuildTarget
	for rows.Next() {
		var t BuildTarget
		if err := rows.Scan(&t.TargetKey, &t.SpecHash, &t.ProjectID, &t.EpicID,
			&t.EpicState, &t.ProjectKey, &t.Name, &t.Actor); err != nil {
			return nil, fmt.Errorf("scan pending target: %w", err)
		}
		out = append(out, t)
	}
	// Checked, because a cursor that fails mid-iteration otherwise returns a
	// SHORT list that reads exactly like "nothing left to recover".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending targets: %w", err)
	}
	return out, nil
}

// TicketMigration records the tickets a decomposition produced.
//
// Without it a popped ticket is an id and a title, and its acceptance criteria
// — which are the whole of what the product owner validates against — exist
// only in the tracker's issue body. Recording them here is what lets a run
// built from a pop be validated at all.
const TicketMigration = `
CREATE TABLE planned_ticket (
    issue      TEXT PRIMARY KEY,
    target_key TEXT NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    criteria   TEXT NOT NULL DEFAULT '[]'
)`

// SQLTickets persists planned tickets in SQLite.
type SQLTickets struct{ DB *sql.DB }

// Put records a ticket a decomposition produced.
func (s SQLTickets) Put(ctx context.Context, targetKey string, t Ticket) error {
	criteria, err := json.Marshal(t.Criteria)
	if err != nil {
		return fmt.Errorf("marshal criteria: %w", err)
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO planned_ticket (issue, target_key, title, body, criteria)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(issue) DO UPDATE SET
		   target_key = excluded.target_key, title = excluded.title,
		   body = excluded.body, criteria = excluded.criteria`,
		t.IssueID, targetKey, t.Title, t.Body, string(criteria))
	if err != nil {
		return fmt.Errorf("record planned ticket: %w", err)
	}
	return nil
}

// Find reads a ticket by the issue it became.
func (s SQLTickets) Find(ctx context.Context, issue string) (Ticket, bool, error) {
	t := Ticket{IssueID: issue}
	var criteria string
	err := s.DB.QueryRowContext(ctx,
		`SELECT title, body, criteria FROM planned_ticket WHERE issue = ?`, issue).
		Scan(&t.Title, &t.Body, &criteria)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, fmt.Errorf("read planned ticket: %w", err)
	}
	if err := json.Unmarshal([]byte(criteria), &t.Criteria); err != nil {
		return Ticket{}, false, fmt.Errorf("unmarshal criteria: %w", err)
	}
	return t, true, nil
}
