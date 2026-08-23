package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Migration is workspace's schema. Each module owns its own tables.
const Migration = `
CREATE TABLE workspace (
    run     TEXT PRIMARY KEY,
    ticket  TEXT NOT NULL,
    path    TEXT NOT NULL,
    branch  TEXT NOT NULL,
    base    TEXT NOT NULL,
    state   TEXT NOT NULL,
    cleanup TEXT NOT NULL DEFAULT ''
)`

// RemovalMigration records when a worktree went away.
//
// A timestamp rather than a third state: ENT-workspace declares exactly two,
// and a "removed" state would make "was this ever created?" — the question
// recovery turns on — unanswerable for a row that has since been cleaned up.
const RemovalMigration = `
ALTER TABLE workspace ADD COLUMN removed_at TEXT NOT NULL DEFAULT ''`

// SQLStore stores workspaces in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert writes a workspace row.
func (s SQLStore) Upsert(ctx context.Context, w Workspace) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO workspace (run, ticket, path, branch, base, state, cleanup, removed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run) DO UPDATE SET
		   ticket = excluded.ticket, path = excluded.path, branch = excluded.branch,
		   base = excluded.base, state = excluded.state, cleanup = excluded.cleanup,
		   removed_at = excluded.removed_at`,
		w.Run, w.Ticket, w.Path, w.Branch, w.Base, w.State, w.CleanupError,
		stampOf(w.RemovedAt))
	if err != nil {
		return fmt.Errorf("upsert workspace: %w", err)
	}
	return nil
}

// Find reads a workspace by run.
func (s SQLStore) Find(ctx context.Context, run string) (Workspace, bool, error) {
	w := Workspace{Run: run}
	var removed string
	err := s.DB.QueryRowContext(ctx,
		`SELECT ticket, path, branch, base, state, cleanup, removed_at
		   FROM workspace WHERE run = ?`, run).
		Scan(&w.Ticket, &w.Path, &w.Branch, &w.Base, &w.State, &w.CleanupError, &removed)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, false, nil
	}
	if err != nil {
		return Workspace{}, false, fmt.Errorf("read workspace: %w", err)
	}
	if w.RemovedAt, err = parseStamp(removed); err != nil {
		return Workspace{}, false, err
	}
	return w, true, nil
}

// Pending lists workspaces a crash left mid-creation.
func (s SQLStore) Pending(ctx context.Context) ([]Workspace, error) {
	return s.inState(ctx, StatePending)
}

// Created lists workspaces whose creation was proven and which are still
// expected to be on disk. A removed one is not a discrepancy waiting to be
// found — it is one the operator already resolved.
func (s SQLStore) Created(ctx context.Context) ([]Workspace, error) {
	return s.inState(ctx, StateCreated)
}

func (s SQLStore) inState(ctx context.Context, state string) ([]Workspace, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT run, ticket, path, branch, base, state, cleanup, removed_at
		 FROM workspace WHERE state = ? AND removed_at = '' ORDER BY run`, state)
	if err != nil {
		return nil, fmt.Errorf("query %s workspaces: %w", state, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Workspace
	for rows.Next() {
		var w Workspace
		var removed string
		if err := rows.Scan(&w.Run, &w.Ticket, &w.Path, &w.Branch,
			&w.Base, &w.State, &w.CleanupError, &removed); err != nil {
			return nil, fmt.Errorf("scan %s workspace: %w", state, err)
		}
		if w.RemovedAt, err = parseStamp(removed); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list that reads exactly like "nothing left to recover".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s workspaces: %w", state, err)
	}
	return out, nil
}

// stampOf renders a time, using the empty string for "never".
//
// A sentinel rather than a nullable column: every read then gets a string
// back, and "not removed" has one spelling instead of two.
func stampOf(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseStamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse removal stamp %q: %w", s, err)
	}
	return t, nil
}
