package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// SQLStore stores workspaces in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert writes a workspace row.
func (s SQLStore) Upsert(ctx context.Context, w Workspace) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO workspace (run, ticket, path, branch, base, state, cleanup)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run) DO UPDATE SET
		   ticket = excluded.ticket, path = excluded.path, branch = excluded.branch,
		   base = excluded.base, state = excluded.state, cleanup = excluded.cleanup`,
		w.Run, w.Ticket, w.Path, w.Branch, w.Base, w.State, w.Cleanup)
	if err != nil {
		return fmt.Errorf("upsert workspace: %w", err)
	}
	return nil
}

// Find reads a workspace by run.
func (s SQLStore) Find(ctx context.Context, run string) (Workspace, bool, error) {
	w := Workspace{Run: run}
	err := s.DB.QueryRowContext(ctx,
		`SELECT ticket, path, branch, base, state, cleanup FROM workspace WHERE run = ?`, run).
		Scan(&w.Ticket, &w.Path, &w.Branch, &w.Base, &w.State, &w.Cleanup)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, false, nil
	}
	if err != nil {
		return Workspace{}, false, fmt.Errorf("read workspace: %w", err)
	}
	return w, true, nil
}

// Pending lists workspaces a crash left mid-creation.
func (s SQLStore) Pending(ctx context.Context) ([]Workspace, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT run, ticket, path, branch, base, state, cleanup
		 FROM workspace WHERE state = ? ORDER BY run`, StatePending)
	if err != nil {
		return nil, fmt.Errorf("query pending workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.Run, &w.Ticket, &w.Path, &w.Branch,
			&w.Base, &w.State, &w.Cleanup); err != nil {
			return nil, fmt.Errorf("scan pending workspace: %w", err)
		}
		out = append(out, w)
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list that reads exactly like "nothing left to recover".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending workspaces: %w", err)
	}
	return out, nil
}
