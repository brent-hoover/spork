package planner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ClaimMigration is the build-completion claim's schema.
//
// One row per target: a claim is the CURRENT attempt, and the whole field set
// rotates together when the next attempt's review-submitting transaction
// replaces it. Keeping history here would invite a recovery into adopting a
// spent claim, which the epoch-scoped key exists to prevent.
const ClaimMigration = `
CREATE TABLE completion_claim (
    target_key       TEXT PRIMARY KEY,
    state            TEXT NOT NULL,
    completion_epoch INTEGER NOT NULL DEFAULT 0,
    submission_key   TEXT NOT NULL DEFAULT '',
    report_key       TEXT NOT NULL DEFAULT '',
    pending_report   TEXT NOT NULL DEFAULT '',
    report_doc       TEXT NOT NULL DEFAULT '',
    report_version   TEXT NOT NULL DEFAULT '',
    review_id        TEXT NOT NULL DEFAULT '',
    review_revision  INTEGER NOT NULL DEFAULT 0,
    subtree_revision INTEGER NOT NULL DEFAULT 0
)`

// CloseMigration records what an epic close is spent on.
//
// A separate migration, because ClaimMigration has already shipped: a database
// that ran it never runs it again, so an edit would leave existing databases
// without these columns and every claim query against one would fail.
const CloseMigration = `
ALTER TABLE completion_claim ADD COLUMN approval_event TEXT NOT NULL DEFAULT '';
ALTER TABLE completion_claim ADD COLUMN close_key TEXT NOT NULL DEFAULT ''`

// ReopenOwedMigration records a close whose outcome nothing resolved.
//
// Its own migration: CloseMigration has shipped, and a database that ran it
// never runs it again.
const ReopenOwedMigration = `
ALTER TABLE completion_claim ADD COLUMN reopen_owed INTEGER NOT NULL DEFAULT 0`

// WatermarkMigration records the feed position a claim was captured at.
//
// Its own migration, for the same reason as every other addition here: the
// claim's table has shipped.
const WatermarkMigration = `
ALTER TABLE completion_claim ADD COLUMN watermark TEXT NOT NULL DEFAULT ''`

// claimColumns is every column a CompletionClaim reads back, in scan order.
const claimColumns = `state, completion_epoch, submission_key, report_key,
	pending_report, report_doc, report_version, review_id, review_revision,
	subtree_revision, approval_event, close_key, reopen_owed, watermark`

// SQLClaims persists completion claims in SQLite.
type SQLClaims struct{ DB *sql.DB }

// Upsert writes a claim.
func (s SQLClaims) Upsert(ctx context.Context, c CompletionClaim) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO completion_claim (target_key, state, completion_epoch,
		   submission_key, report_key, pending_report, report_doc, report_version,
		   review_id, review_revision, subtree_revision, approval_event, close_key,
		   reopen_owed, watermark)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(target_key) DO UPDATE SET
		   state = excluded.state, completion_epoch = excluded.completion_epoch,
		   submission_key = excluded.submission_key, report_key = excluded.report_key,
		   pending_report = excluded.pending_report, report_doc = excluded.report_doc,
		   report_version = excluded.report_version, review_id = excluded.review_id,
		   review_revision = excluded.review_revision,
		   subtree_revision = excluded.subtree_revision,
		   approval_event = excluded.approval_event, close_key = excluded.close_key,
		   watermark = excluded.watermark,
		   -- NEVER cleared by a later attempt. An unresolved reopen
		   -- obligation outlives the claim that incurred it: the epic may be
		   -- closed over work that returned, and the next attempt knowing
		   -- nothing about it does not make that untrue.
		   reopen_owed = completion_claim.reopen_owed OR excluded.reopen_owed`,
		c.TargetKey, c.State, c.Epoch, c.SubmissionKey, c.ReportKey, c.PendingReport,
		c.ReportDoc, c.ReportVersion, c.ReviewID, c.ReviewRevision, c.SubtreeRevision,
		c.ApprovalEvent, c.CloseKey, c.ReopenOwed, c.Watermark)
	if err != nil {
		return fmt.Errorf("upsert completion claim: %w", err)
	}
	return nil
}

// Find reads a target's current claim.
func (s SQLClaims) Find(ctx context.Context, targetKey string) (CompletionClaim, bool, error) {
	c := CompletionClaim{TargetKey: targetKey}
	err := s.DB.QueryRowContext(ctx,
		`SELECT `+claimColumns+` FROM completion_claim WHERE target_key = ?`, targetKey).
		Scan(&c.State, &c.Epoch, &c.SubmissionKey, &c.ReportKey, &c.PendingReport,
			&c.ReportDoc, &c.ReportVersion, &c.ReviewID, &c.ReviewRevision,
			&c.SubtreeRevision, &c.ApprovalEvent, &c.CloseKey, &c.ReopenOwed, &c.Watermark)
	if errors.Is(err, sql.ErrNoRows) {
		return CompletionClaim{}, false, nil
	}
	if err != nil {
		return CompletionClaim{}, false, fmt.Errorf("read completion claim: %w", err)
	}
	return c, true, nil
}

// Closing lists claims a crash left mid-close.
//
// Replayed under the PERSISTED close key: a close that landed returns its
// original success, never a close-used conflict, because that very key
// stamped it.
func (s SQLClaims) Closing(ctx context.Context) ([]CompletionClaim, error) {
	return s.inState(ctx, CompletionClosing)
}

// Submitting lists claims a crash left mid-submission.
func (s SQLClaims) Submitting(ctx context.Context) ([]CompletionClaim, error) {
	return s.inState(ctx, CompletionSubmitting)
}

// inState lists claims resting at one lifecycle state.
func (s SQLClaims) inState(ctx context.Context, state string) ([]CompletionClaim, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT target_key, `+claimColumns+` FROM completion_claim
		   WHERE state = ? ORDER BY target_key`, state)
	if err != nil {
		return nil, fmt.Errorf("query %s claims: %w", state, err)
	}
	defer func() { _ = rows.Close() }()

	var out []CompletionClaim
	for rows.Next() {
		var c CompletionClaim
		if err := rows.Scan(&c.TargetKey, &c.State, &c.Epoch, &c.SubmissionKey,
			&c.ReportKey, &c.PendingReport, &c.ReportDoc, &c.ReportVersion,
			&c.ReviewID, &c.ReviewRevision, &c.SubtreeRevision,
			&c.ApprovalEvent, &c.CloseKey, &c.ReopenOwed, &c.Watermark); err != nil {
			return nil, fmt.Errorf("scan completion claim: %w", err)
		}
		out = append(out, c)
	}
	// Checked, because a cursor failing mid-iteration returns a SHORT list —
	// which here reads as "every completion review reached sutra".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate completion claims: %w", err)
	}
	return out, nil
}
