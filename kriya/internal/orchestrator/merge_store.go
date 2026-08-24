package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MergeMigration is the merge queue's schema.
//
// attempt_key is the PRIMARY KEY, which is what makes a replayed approval
// event collide instead of duplicating. Nothing above this has to check
// first — the uniqueness IS the check.
const MergeMigration = `
CREATE TABLE merge_attempt (
    attempt_key    TEXT PRIMARY KEY,
    target_key     TEXT NOT NULL,
    build          TEXT NOT NULL,
    review         TEXT NOT NULL,
    revision       INTEGER NOT NULL,
    approval_event TEXT NOT NULL,
    commit_sha     TEXT NOT NULL DEFAULT '',
    expected_base  TEXT NOT NULL DEFAULT '',
    consume_key    TEXT NOT NULL DEFAULT '',
    merge_commit   TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL,
    note           TEXT NOT NULL DEFAULT '',
    seq            INTEGER NOT NULL
)`

// ResourceMigration scopes serialization to a repository and branch.
//
// A SEPARATE migration rather than a column added to the one above. That
// migration is already registered, so a database that ran it never runs it
// again: editing it in place left existing databases with no resource column
// at all, and every merge-attempt query against one failed. Fresh and existing
// databases now take the same two steps and end identical.
const ResourceMigration = `
ALTER TABLE merge_attempt ADD COLUMN resource TEXT NOT NULL DEFAULT ''`

// attemptColumns is every column a MergeAttempt reads back, in scan order.
const attemptColumns = `attempt_key, target_key, build, review, revision, approval_event,
	commit_sha, resource, expected_base, consume_key, merge_commit, state, note`

// SQLAttempts persists merge attempts in SQLite.
type SQLAttempts struct{ DB *sql.DB }

// Insert enqueues an attempt, reporting whether it was new.
//
// A DO NOTHING conflict rather than a read-then-write: two approvals racing
// would both find nothing and both insert, and the second would fail on the
// key anyway — so the key is asked to decide, once.
func (s SQLAttempts) Insert(ctx context.Context, a MergeAttempt) (bool, error) {
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO merge_attempt (attempt_key, target_key, build, review, revision,
		   approval_event, commit_sha, resource, expected_base, state, seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		   (SELECT COALESCE(MAX(seq), 0) + 1 FROM merge_attempt))
		 ON CONFLICT(attempt_key) DO NOTHING`,
		a.Key, a.TargetKey, a.Build, a.Review, a.Revision, a.ApprovalEvent,
		a.Commit, a.Resource, a.ExpectedBase, a.State)
	if err != nil {
		return false, fmt.Errorf("insert merge attempt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count inserted attempts: %w", err)
	}
	return n == 1, nil
}

// Update advances an attempt's step.
func (s SQLAttempts) Update(ctx context.Context, a MergeAttempt) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE merge_attempt SET expected_base = ?, consume_key = ?, merge_commit = ?,
		   state = ?, note = ? WHERE attempt_key = ?`,
		a.ExpectedBase, a.ConsumeKey, a.MergeCommit, a.State, a.Note, a.Key)
	if err != nil {
		return fmt.Errorf("update merge attempt: %w", err)
	}
	return nil
}

// Find reads an attempt by key.
func (s SQLAttempts) Find(ctx context.Context, key string) (MergeAttempt, bool, error) {
	var a MergeAttempt
	err := s.DB.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM merge_attempt WHERE attempt_key = ?`, key).
		Scan(&a.Key, &a.TargetKey, &a.Build, &a.Review, &a.Revision, &a.ApprovalEvent,
			&a.Commit, &a.Resource, &a.ExpectedBase, &a.ConsumeKey, &a.MergeCommit,
			&a.State, &a.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return MergeAttempt{}, false, nil
	}
	if err != nil {
		return MergeAttempt{}, false, fmt.Errorf("read merge attempt: %w", err)
	}
	return a, true, nil
}

// Unfinished lists attempts a crash left mid-flight, oldest first.
//
// Insertion order, not key order: the queue is a queue, and a target's
// approvals must land in the order they arrived.
func (s SQLAttempts) Unfinished(ctx context.Context) ([]MergeAttempt, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+attemptColumns+` FROM merge_attempt
		   WHERE state NOT IN (?, ?) ORDER BY seq`, AttemptMerged, AttemptAborted)
	if err != nil {
		return nil, fmt.Errorf("query unfinished merge attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []MergeAttempt
	for rows.Next() {
		var a MergeAttempt
		if err := rows.Scan(&a.Key, &a.TargetKey, &a.Build, &a.Review, &a.Revision,
			&a.ApprovalEvent, &a.Commit, &a.Resource, &a.ExpectedBase, &a.ConsumeKey,
			&a.MergeCommit, &a.State, &a.Note); err != nil {
			return nil, fmt.Errorf("scan merge attempt: %w", err)
		}
		out = append(out, a)
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list that reads exactly like "every merge finished".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate merge attempts: %w", err)
	}
	return out, nil
}
