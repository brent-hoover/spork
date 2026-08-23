package reviewbridge

import (
	"context"
	"database/sql"
	"fmt"
)

// Migration is the review bridge's schema.
//
// enqueue_attempt is written BEFORE roborev is called, so a crash in the
// window between the call and its result leaves a row saying a review may be
// running — which is what recovery reconciles. The round is keyed by its own
// id and never by commit sha: two rounds can share a sha, and adopting a job
// by sha would let kriya act on a review it did not ask for (R1).
const Migration = `
CREATE TABLE enqueue_attempt (
    round  TEXT PRIMARY KEY,
    run    TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    state  TEXT NOT NULL,
    job_id INTEGER NOT NULL DEFAULT 0,
    note   TEXT NOT NULL DEFAULT ''
);
CREATE TABLE review_round (
    id       TEXT PRIMARY KEY,
    run      TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    job_id   INTEGER NOT NULL,
    verdict  TEXT NOT NULL,
    findings TEXT NOT NULL DEFAULT ''
)`

// SQLStore persists rounds and attempts in SQLite.
type SQLStore struct{ DB *sql.DB }

// UpsertAttempt records an enqueue attempt and its outcome.
func (s SQLStore) UpsertAttempt(ctx context.Context, a EnqueueAttempt) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO enqueue_attempt (round, run, commit_sha, state, job_id, note)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(round) DO UPDATE SET
		   state = excluded.state, job_id = excluded.job_id, note = excluded.note`,
		a.Round, a.Run, a.Commit, a.State, a.JobID, a.Note)
	if err != nil {
		return fmt.Errorf("upsert enqueue attempt: %w", err)
	}
	return nil
}

// UpsertRound records a round and its verdict.
func (s SQLStore) UpsertRound(ctx context.Context, r Round) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO review_round (id, run, commit_sha, job_id, verdict, findings)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   job_id = excluded.job_id, verdict = excluded.verdict,
		   findings = excluded.findings`,
		r.ID, r.Run, r.Commit, r.JobID, r.Verdict, r.Findings)
	if err != nil {
		return fmt.Errorf("upsert review round: %w", err)
	}
	return nil
}

// Unresolved returns attempts that may have reached roborev without kriya
// learning the outcome.
//
// Pending counts as unresolved here. A row is written before the call and
// updated after it, so a row still reading "pending" when recovery runs is a
// crash in exactly that window — the same ambiguity as an outright failure,
// and recovery runs before any new work, so no live attempt can be caught.
func (s SQLStore) Unresolved(ctx context.Context) ([]EnqueueAttempt, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT round, run, commit_sha, state, job_id, note
		   FROM enqueue_attempt WHERE state IN (?, ?)`,
		AttemptUnresolved, AttemptPending)
	if err != nil {
		return nil, fmt.Errorf("query unresolved attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EnqueueAttempt
	for rows.Next() {
		var a EnqueueAttempt
		if err := rows.Scan(&a.Round, &a.Run, &a.Commit, &a.State, &a.JobID, &a.Note); err != nil {
			return nil, fmt.Errorf("scan unresolved attempt: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read unresolved attempts: %w", err)
	}
	return out, nil
}
