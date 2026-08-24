package orchestrator

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// StallMigration is the stall inbox's schema.
//
// stall_key is the PRIMARY KEY, which is what makes re-detection converge: a
// poll, a second detector and a restart all insert the same key, and the
// uniqueness IS the convergence.
const StallMigration = `
CREATE TABLE stall (
    stall_key  TEXT PRIMARY KEY,
    target_key TEXT NOT NULL,
    cause      TEXT NOT NULL,
    state      TEXT NOT NULL,
    created    TEXT NOT NULL,
    resolved   TEXT NOT NULL DEFAULT ''
)`

// SQLStalls persists stalls in SQLite.
type SQLStalls struct{ DB *sql.DB }

// Insert records a stall, or reopens the one already under its key.
//
// A DO UPDATE that touches only the lifecycle, never the cause: a condition
// that came back is the same condition, and rewriting what the operator is
// reading would lose the diagnosis they are working from.
func (s SQLStalls) Insert(ctx context.Context, st Stall) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO stall (stall_key, target_key, cause, state, created, resolved)
		 VALUES (?, ?, ?, ?, ?, '')
		 ON CONFLICT(stall_key) DO UPDATE SET state = ?, resolved = ''`,
		st.Key, st.TargetKey, st.Cause, st.State, st.Created.UTC().Format(time.RFC3339Nano),
		StallOpen)
	if err != nil {
		return fmt.Errorf("insert stall: %w", err)
	}
	return nil
}

// Resolve stamps a stall, leaving its cause and creation standing.
func (s SQLStalls) Resolve(ctx context.Context, key string, at time.Time) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE stall SET state = ?, resolved = ? WHERE stall_key = ?`,
		StallResolved, at.UTC().Format(time.RFC3339Nano), key)
	if err != nil {
		return fmt.Errorf("resolve stall: %w", err)
	}
	return nil
}

// Open lists the stalls the operator inbox shows.
func (s SQLStalls) Open(ctx context.Context) ([]Stall, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT stall_key, target_key, cause, created FROM stall
		   WHERE state = ? ORDER BY created, stall_key`, StallOpen)
	if err != nil {
		return nil, fmt.Errorf("query open stalls: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Stall
	for rows.Next() {
		st := Stall{State: StallOpen}
		var created string
		if err := rows.Scan(&st.Key, &st.TargetKey, &st.Cause, &created); err != nil {
			return nil, fmt.Errorf("scan stall: %w", err)
		}
		st.Created, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse stall time: %w", err)
		}
		out = append(out, st)
	}
	// Checked, because a cursor failing mid-iteration returns a SHORT list —
	// which here reads as an inbox with fewer stalls than there are.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stalls: %w", err)
	}
	return out, nil
}
