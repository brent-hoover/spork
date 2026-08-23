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

// ResponseMigration adds the response lifecycle.
//
// A second migration rather than an edit to the first: the ledger records
// which migrations ran, and rewriting an applied one leaves every existing
// database claiming to have columns it does not have.
const ResponseMigration = `
ALTER TABLE review_round ADD COLUMN state TEXT NOT NULL DEFAULT 'open';
ALTER TABLE review_round ADD COLUMN response TEXT NOT NULL DEFAULT ''`

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
		`INSERT INTO review_round (id, run, commit_sha, job_id, verdict, findings, state, response)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   job_id = excluded.job_id, verdict = excluded.verdict,
		   findings = excluded.findings, state = excluded.state,
		   response = excluded.response`,
		r.ID, r.Run, r.Commit, r.JobID, r.Verdict, r.Findings, r.State, r.Response)
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

// Unsettled lists rounds whose response lifecycle a crash left mid-flight.
//
// The state and its prepared payload are written in one statement, so a row
// reading "commenting" carries exactly the response that was going to be sent.
func (s SQLStore) Unsettled(ctx context.Context) ([]Round, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, run, commit_sha, job_id, verdict, findings, state, response
		   FROM review_round WHERE state IN (?, ?) ORDER BY id`,
		RoundCommenting, RoundClosing)
	if err != nil {
		return nil, fmt.Errorf("query unsettled rounds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Round
	for rows.Next() {
		var r Round
		if err := rows.Scan(&r.ID, &r.Run, &r.Commit, &r.JobID, &r.Verdict,
			&r.Findings, &r.State, &r.Response); err != nil {
			return nil, fmt.Errorf("scan unsettled round: %w", err)
		}
		out = append(out, r)
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list that reads exactly like "no round left open".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unsettled rounds: %w", err)
	}
	return out, nil
}
