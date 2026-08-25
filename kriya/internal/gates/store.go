package gates

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Migration is the gate-result schema.
//
// The unique key is (build, module, gate, attempt): results upsert rather than
// accumulate, so a rerun of the same round replaces its result and never
// duplicates, while a NEW round keeps its own.
const Migration = `
CREATE TABLE gate_result (
    build   TEXT NOT NULL,
    module  TEXT NOT NULL,
    gate    TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    commit_sha TEXT NOT NULL,
    passed  INTEGER NOT NULL,
    detail  TEXT NOT NULL,
    PRIMARY KEY (build, module, gate, attempt)
)`

// SQLStore stores gate results in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert records a result.
func (s SQLStore) Upsert(ctx context.Context, r Result) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO gate_result (build, module, gate, attempt, commit_sha, passed, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(build, module, gate, attempt) DO UPDATE SET
		   commit_sha = excluded.commit_sha, passed = excluded.passed,
		   detail = excluded.detail`,
		r.Build, r.Module, r.Gate, r.Attempt, r.Commit, r.Passed, string(r.Detail))
	if err != nil {
		return fmt.Errorf("upsert gate result: %w", err)
	}
	return nil
}

// Passed reports whether a gate passed at this exact commit under this
// attempt.
//
// Both are in the WHERE clause, which is what makes a stale pass unusable: a
// result recorded at an older commit does not match, and neither does one from
// a previous attempt — which matters because integrating a new base can leave
// the branch head unchanged.
func (s SQLStore) Passed(
	ctx context.Context, build, module, gate, commit string, attempt int,
) (bool, error) {
	passed, _, err := s.Outcome(ctx, build, module, gate, commit, attempt)
	return passed, err
}

// Outcome reports a gate's result AND whether it ran at all.
//
// The chain only needs "did it pass", and absence answers that: a gate never
// run has not passed. An operator needs the other bit too — a gate that has
// not run yet is not a gate that failed, and showing six FAILED rows for a
// chain that reached the second one is a lie about where the run is.
func (s SQLStore) Outcome(
	ctx context.Context, build, module, gate, commit string, attempt int,
) (passed, ran bool, err error) {
	err = s.DB.QueryRowContext(ctx,
		`SELECT passed FROM gate_result
		 WHERE build = ? AND module = ? AND gate = ? AND commit_sha = ? AND attempt = ?`,
		build, module, gate, commit, attempt).Scan(&passed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		// A store that cannot be read is not a gate that has not run. The
		// chain's Passed swallows this deliberately — absence and failure are
		// the same answer to it — but a REPORT must not claim a position it
		// could not read.
		return false, false, fmt.Errorf("read %s result: %w", gate, err)
	}
	return passed, true, nil
}
