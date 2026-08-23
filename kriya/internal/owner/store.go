package owner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Migration is the owner's schema.
//
// Keyed by build AND attempt: a rerun after integration needs a fresh PO pass,
// and a single row per build would let the previous attempt's verdict stand in
// for one nobody made.
const Migration = `
CREATE TABLE validation (
    build     TEXT NOT NULL,
    attempt   INTEGER NOT NULL,
    verdict   TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    notes     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (build, attempt)
)`

// SQLStore persists validations in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert records a verdict.
func (s SQLStore) Upsert(ctx context.Context, v Validation) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO validation (build, attempt, verdict, commit_sha, notes)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(build, attempt) DO UPDATE SET
		   verdict = excluded.verdict, commit_sha = excluded.commit_sha,
		   notes = excluded.notes`,
		v.Build, v.Attempt, v.Verdict, v.Commit, v.Notes)
	if err != nil {
		return fmt.Errorf("upsert validation: %w", err)
	}
	return nil
}

// Find reads the verdict for a build's gate attempt.
func (s SQLStore) Find(ctx context.Context, build string, attempt int) (Validation, bool, error) {
	v := Validation{Build: build, Attempt: attempt}
	err := s.DB.QueryRowContext(ctx,
		`SELECT verdict, commit_sha, notes FROM validation
		   WHERE build = ? AND attempt = ?`, build, attempt).
		Scan(&v.Verdict, &v.Commit, &v.Notes)
	if errors.Is(err, sql.ErrNoRows) {
		return Validation{}, false, nil
	}
	if err != nil {
		return Validation{}, false, fmt.Errorf("read validation: %w", err)
	}
	return v, true, nil
}
