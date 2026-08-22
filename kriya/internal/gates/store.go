package gates

import (
	"context"
	"database/sql"
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

// Passed reports whether a gate passed at this exact commit.
//
// The commit is in the WHERE clause, which is what makes a stale pass
// unusable: a result recorded at an older commit simply does not match.
func (s SQLStore) Passed(ctx context.Context, build, module, gate, commit string) (bool, error) {
	var passed bool
	err := s.DB.QueryRowContext(ctx,
		`SELECT passed FROM gate_result
		 WHERE build = ? AND module = ? AND gate = ? AND commit_sha = ?
		 ORDER BY attempt DESC LIMIT 1`, build, module, gate, commit).Scan(&passed)
	if err != nil {
		// Absence is not failure: a gate never run at this commit has not
		// passed, which is the answer the chain needs.
		return false, nil
	}
	return passed, nil
}
