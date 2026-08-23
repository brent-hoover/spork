package architect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Migration is the architect's schema.
const Migration = `
CREATE TABLE intervention (
    build     TEXT PRIMARY KEY,
    trigger   TEXT NOT NULL,
    findings  TEXT NOT NULL DEFAULT '[]',
    direction TEXT NOT NULL DEFAULT '',
    state     TEXT NOT NULL,
    outcome   TEXT NOT NULL DEFAULT ''
)`

// SQLStore persists interventions in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert records an intervention.
func (s SQLStore) Upsert(ctx context.Context, i Intervention) error {
	findings := string(i.Findings)
	if findings == "" {
		findings = "[]"
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO intervention (build, trigger, findings, direction, state, outcome)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(build) DO UPDATE SET
		   trigger = excluded.trigger, findings = excluded.findings,
		   direction = excluded.direction, state = excluded.state,
		   outcome = excluded.outcome`,
		i.Build, i.Trigger, findings, i.Direction, i.State, i.Outcome)
	if err != nil {
		return fmt.Errorf("upsert intervention: %w", err)
	}
	return nil
}

// Find reads a run's intervention.
func (s SQLStore) Find(ctx context.Context, build string) (Intervention, bool, error) {
	i := Intervention{Build: build}
	var findings string
	err := s.DB.QueryRowContext(ctx,
		`SELECT trigger, findings, direction, state, outcome
		   FROM intervention WHERE build = ?`, build).
		Scan(&i.Trigger, &findings, &i.Direction, &i.State, &i.Outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return Intervention{}, false, nil
	}
	if err != nil {
		return Intervention{}, false, fmt.Errorf("read intervention: %w", err)
	}
	i.Findings = []byte(findings)
	return i, true, nil
}
