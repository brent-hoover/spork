package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"kriya/internal/planner"
)

// Migration is orchestrator's schema.
const Migration = `
CREATE TABLE build_run (
    id         TEXT PRIMARY KEY,
    ticket     TEXT NOT NULL,
    plan       TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL,
    gated_base TEXT NOT NULL DEFAULT '',
    error      TEXT NOT NULL DEFAULT '',
    attempt    INTEGER NOT NULL DEFAULT 0
)`

// RoundLimitMigration snapshots the pair loop's round limit onto the run.
//
// A separate migration for the same reason as every other second one: the
// ledger records which ran, and rewriting an applied one leaves existing
// databases claiming a column they do not have.
const RoundLimitMigration = `
ALTER TABLE build_run ADD COLUMN round_limit INTEGER NOT NULL DEFAULT 0`

// SubmissionMigration adds the review submission's write-ahead columns.
const SubmissionMigration = `
ALTER TABLE build_run ADD COLUMN review_key TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN review_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN review_session TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN review_state TEXT NOT NULL DEFAULT 'none';
ALTER TABLE build_run ADD COLUMN review_id TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN review_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE build_run ADD COLUMN review_verdict_event TEXT NOT NULL DEFAULT ''`

// CompletionMigration adds the ticket close's write-ahead columns.
const CompletionMigration = `
ALTER TABLE build_run ADD COLUMN close_key TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN completed_head TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN completion_state TEXT NOT NULL DEFAULT 'none'`

// HeadMigration records the branch head — the developed work.
//
// Separate from gated_base, which is the commit the branch was cut from. The
// two were one column, which meant the chain gated the base and the review
// named it: the work was never looked at.
const HeadMigration = `
ALTER TABLE build_run ADD COLUMN head TEXT NOT NULL DEFAULT ''`

// StartedMigration records when a run began.
//
// Its own migration: the run's table has shipped, and a database that ran that
// migration never runs it again.
const StartedMigration = `
ALTER TABLE build_run ADD COLUMN started TEXT NOT NULL DEFAULT ''`

// FindingMigration records a spike run's kind and its documented finding.
//
// Without the kind a popped spike is indistinguishable from implementation
// work and goes through the code gate chain — which fails it for having no
// tests, a finding about the ticket's shape rather than about the risk.
const FindingMigration = `
ALTER TABLE build_run ADD COLUMN kind TEXT NOT NULL DEFAULT 'implementation';
ALTER TABLE build_run ADD COLUMN finding_doc TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN finding_version TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN finding_key TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN pending_finding TEXT NOT NULL DEFAULT ''`

// PopKeyMigration records the key a run's claim was made under.
//
// Its own migration because IssueMigration has shipped. The key is written
// before the claim, so a crash in that window leaves a run that can replay its
// own pop — sutra settles a claim under its key, so the replay returns the same
// ticket rather than taking a second, and an explicitly empty replay says no
// work was ever claimed.
const PopKeyMigration = `
ALTER TABLE build_run ADD COLUMN pop_key TEXT NOT NULL DEFAULT ''`

// IssueMigration records the tracker issue and branch the run works on.
//
// Recovery replays a submission or a close from the run's PERSISTED fields.
// It had neither, and substituted the ticket TITLE for the issue id and an
// empty branch — so a replay asked sutra to open a review against an issue
// that does not exist, on no branch.
const IssueMigration = `
ALTER TABLE build_run ADD COLUMN issue TEXT NOT NULL DEFAULT '';
ALTER TABLE build_run ADD COLUMN branch TEXT NOT NULL DEFAULT ''`

// runColumns is every column a BuildRun reads back, in scan order.
const runColumns = `ticket, issue, branch, plan, state, head, gated_base, error, attempt, round_limit,
	pop_key,
	started, kind, finding_doc, finding_version, finding_key, pending_finding,
	review_key, review_commit, review_session, review_state, review_id,
	review_revision, review_verdict_event, close_key, completed_head, completion_state`

// SQLStore stores build runs in SQLite.
type SQLStore struct{ DB *sql.DB }

// Reserve writes the run and takes an admission slot in ONE transaction.
//
// The two are one fact. Admitted separately, a replacement can raise the fence
// between the admission and the claim — and the claim then lands inside the
// unactivated window with no durable run for retirement to reconcile. Written
// separately, a crash between them leaves a claim nothing owns.
//
// The run is written QUEUED with its pop key and no issue: that shape is
// precisely "a claim may have been made and we do not yet know what it got",
// which is what recovery looks for.
func (s SQLStore) Reserve(
	ctx context.Context, r BuildRun, guard Admission, readVersion int,
) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := guard.AdmitWithin(ctx, planTx{tx: tx}, readVersion); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO build_run (id, ticket, issue, branch, plan, state, pop_key, round_limit)
		 VALUES (?, '', '', '', ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		r.ID, r.Plan, string(StateQueued), r.PopKey, r.RoundLimit); err != nil {
		return fmt.Errorf("reserve build run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reservation: %w", err)
	}
	return nil
}

// Bind attaches a claim's result to a reservation.
//
// Conditional on the run still being an UNBOUND queued reservation. A replayed
// bind against a run that has since moved on would drag it back to the ticket
// it started with, discarding whatever progress it made.
func (s SQLStore) Bind(ctx context.Context, runID, issue, title string) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE build_run SET issue = ?, ticket = ?
		   WHERE id = ? AND state = ? AND issue = ''`,
		issue, title, runID, string(StateQueued)); err != nil {
		return fmt.Errorf("bind run %s: %w", runID, err)
	}
	return nil
}

// SettleEmpty moves a reservation whose claim returned nothing to no-work.
//
// Conditional for the same reason: only an unbound queued reservation can
// settle this way. No work is invented for it, and nothing about it is
// classified as issued.
func (s SQLStore) SettleEmpty(ctx context.Context, runID string) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE build_run SET state = ?
		   WHERE id = ? AND state = ? AND issue = ''`,
		string(StateNoWork), runID, string(StateQueued)); err != nil {
		return fmt.Errorf("settle run %s as no-work: %w", runID, err)
	}
	return nil
}

// Unbound lists runs that were reserved but never bound to a ticket.
//
// A queued run with a pop key and no issue is a claim whose outcome is
// unknown. Recovery replays the pop under the persisted key: sutra returns the
// same ticket if one was claimed, and an explicitly empty result if none was.
func (s SQLStore) Unbound(ctx context.Context) ([]BuildRun, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, `+runColumns+` FROM build_run
		   WHERE state = ? AND issue = '' AND pop_key <> '' ORDER BY rowid`,
		string(StateQueued))
	if err != nil {
		return nil, fmt.Errorf("query unbound runs: %w", err)
	}
	return scanRuns(rows, "unbound")
}

// planTx adapts a *sql.Tx to the planner's narrow handle.
type planTx struct{ tx *sql.Tx }

func (p planTx) ExecContext(
	ctx context.Context, query string, args ...any,
) (planner.Result, error) {
	return p.tx.ExecContext(ctx, query, args...)
}

func (p planTx) QueryRowContext(ctx context.Context, query string, args ...any) planner.Row {
	return p.tx.QueryRowContext(ctx, query, args...)
}

// Upsert writes a run.
func (s SQLStore) Upsert(ctx context.Context, r BuildRun) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO build_run (id, ticket, issue, branch, plan, state, head, gated_base, error, attempt,
		   round_limit, pop_key, started, kind, finding_doc, finding_version, finding_key,
		   pending_finding,
		   review_key, review_commit, review_session, review_state, review_id,
		   review_revision, review_verdict_event, close_key, completed_head, completion_state)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   ticket = excluded.ticket, issue = excluded.issue, branch = excluded.branch,
		   plan = excluded.plan, state = excluded.state,
		   head = excluded.head, gated_base = excluded.gated_base, error = excluded.error,
		   attempt = excluded.attempt, round_limit = excluded.round_limit,
		   pop_key = excluded.pop_key,
		   started = excluded.started, kind = excluded.kind,
		   finding_doc = excluded.finding_doc,
		   finding_version = excluded.finding_version,
		   finding_key = excluded.finding_key,
		   pending_finding = excluded.pending_finding,
		   review_key = excluded.review_key, review_commit = excluded.review_commit,
		   review_session = excluded.review_session, review_state = excluded.review_state,
		   review_id = excluded.review_id,
		   review_revision = excluded.review_revision,
		   review_verdict_event = excluded.review_verdict_event,
		   close_key = excluded.close_key,
		   completed_head = excluded.completed_head,
		   completion_state = excluded.completion_state`,
		r.ID, r.Ticket, r.Issue, r.Branch, r.Plan, string(r.State), r.Head, r.GatedBase, r.Error, r.Attempt,
		r.RoundLimit, r.PopKey, startedOf(r), kindOf(r), r.FindingDoc, r.FindingVersion,
		r.FindingKey, r.PendingFinding,
		r.ReviewKey, r.ReviewCommit, r.ReviewSession,
		reviewStateOf(r), r.ReviewID, r.ReviewRevision, r.ReviewVerdictEvent,
		r.CloseKey, r.CompletedHead, completionStateOf(r))
	if err != nil {
		return fmt.Errorf("upsert build run: %w", err)
	}
	return nil
}

// Find reads a run by id.
func (s SQLStore) Find(ctx context.Context, id string) (BuildRun, bool, error) {
	r := BuildRun{ID: id}
	var state, started string
	err := s.DB.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM build_run WHERE id = ?`, id).
		Scan(&r.Ticket, &r.Issue, &r.Branch, &r.Plan, &state, &r.Head, &r.GatedBase, &r.Error, &r.Attempt, &r.RoundLimit,
			&r.PopKey, &started, &r.Kind, &r.FindingDoc, &r.FindingVersion, &r.FindingKey,
			&r.PendingFinding,
			&r.ReviewKey, &r.ReviewCommit, &r.ReviewSession, &r.ReviewState, &r.ReviewID,
			&r.ReviewRevision, &r.ReviewVerdictEvent,
			&r.CloseKey, &r.CompletedHead, &r.CompletionState)
	if errors.Is(err, sql.ErrNoRows) {
		return BuildRun{}, false, nil
	}
	if err != nil {
		return BuildRun{}, false, fmt.Errorf("read build run: %w", err)
	}
	r.State = State(state)
	r.Started = startedFrom(started)
	return r, true, nil
}

// kindOf defaults a run with no kind to implementation.
//
// The column is an enum, and a zero-valued BuildRun has none. Implementation
// is the safe default: a spike misfiled as one fails the gate chain loudly,
// where an implementation run misfiled as a spike would SKIP the gates and
// land unreviewed code.
func kindOf(r BuildRun) string {
	if r.Kind == "" {
		return "implementation"
	}
	return r.Kind
}

// startedOf renders a run's start time, defaulting an unset one to blank.
//
// Blank rather than the zero instant: a run recorded before this column
// existed has no start time, and printing year one would be a lie about it.
func startedOf(r BuildRun) string {
	if r.Started.IsZero() {
		return ""
	}
	return r.Started.UTC().Format(time.RFC3339Nano)
}

// startedFrom reads a start time back, treating an unparseable one as absent.
func startedFrom(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return at
}

// reviewStateOf defaults an unset state.
//
// A zero-valued BuildRun has no review state, and writing "" would put a value
// outside the declared enum in the column.
func reviewStateOf(r BuildRun) string {
	if r.ReviewState == "" {
		return SubmitNone
	}
	return r.ReviewState
}

// Submitting lists runs whose review submission a crash left in flight.
func (s SQLStore) Submitting(ctx context.Context) ([]BuildRun, error) {
	return s.inReviewState(ctx, SubmitSubmitting)
}

// Resubmitting lists runs whose rework resubmission a crash left in flight.
func (s SQLStore) Resubmitting(ctx context.Context) ([]BuildRun, error) {
	return s.inReviewState(ctx, SubmitResubmitting)
}

// Completing lists runs whose ticket close a crash left in flight.
func (s SQLStore) Completing(ctx context.Context) ([]BuildRun, error) {
	return s.listRuns(ctx, "completion_state", CompleteCompleting)
}

func (s SQLStore) inReviewState(ctx context.Context, state string) ([]BuildRun, error) {
	return s.listRuns(ctx, "review_state", state)
}

// ForPlan lists every run of a target, newest first.
//
// The whole set, settled ones included: the operator's question is what this
// build has done, and hiding the finished runs answers a different one.
func (s SQLStore) ForPlan(ctx context.Context, plan string) ([]BuildRun, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, `+runColumns+` FROM build_run WHERE plan = ? ORDER BY rowid DESC`, plan)
	if err != nil {
		return nil, fmt.Errorf("query runs for %s: %w", plan, err)
	}
	return scanRuns(rows, plan)
}

// listRuns lists runs whose column holds state.
//
// The column name is a CONSTANT from this file, never caller input: it is
// interpolated because a placeholder cannot name a column, and only a fixed
// set of names ever reaches it.
func (s SQLStore) listRuns(ctx context.Context, column, state string) ([]BuildRun, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, `+runColumns+` FROM build_run WHERE `+column+` = ? ORDER BY id`, state)
	if err != nil {
		return nil, fmt.Errorf("query %s runs: %w", state, err)
	}
	return scanRuns(rows, state)
}

// scanRuns reads a run cursor to exhaustion.
func scanRuns(rows *sql.Rows, what string) ([]BuildRun, error) {
	defer func() { _ = rows.Close() }()

	var out []BuildRun
	for rows.Next() {
		var r BuildRun
		var runState string
		var started string
		if err := rows.Scan(&r.ID, &r.Ticket, &r.Issue, &r.Branch, &r.Plan, &runState, &r.Head, &r.GatedBase, &r.Error,
			&r.Attempt, &r.RoundLimit, &r.PopKey, &started, &r.Kind, &r.FindingDoc,
			&r.FindingVersion, &r.FindingKey, &r.PendingFinding,
			&r.ReviewKey, &r.ReviewCommit,
			&r.ReviewSession, &r.ReviewState, &r.ReviewID,
			&r.ReviewRevision, &r.ReviewVerdictEvent,
			&r.CloseKey, &r.CompletedHead, &r.CompletionState); err != nil {
			return nil, fmt.Errorf("scan %s run: %w", what, err)
		}
		r.State = State(runState)
		r.Started = startedFrom(started)
		out = append(out, r)
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list that reads exactly like "every review reached sutra".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s runs: %w", what, err)
	}
	return out, nil
}

// completionStateOf defaults an unset state.
func completionStateOf(r BuildRun) string {
	if r.CompletionState == "" {
		return CompleteNone
	}
	return r.CompletionState
}

// PopMigration records how many pops a target has settled.
const PopMigration = `
CREATE TABLE pop_ordinal (
    target_key TEXT PRIMARY KEY,
    settled    INTEGER NOT NULL
)`

// SQLOrdinals persists pop ordinals in SQLite.
type SQLOrdinals struct{ DB *sql.DB }

// Current reads how many pops a target has settled. A target that has settled
// none has no row, which is zero rather than an error.
func (s SQLOrdinals) Current(ctx context.Context, targetKey string) (int, error) {
	var settled int
	err := s.DB.QueryRowContext(ctx,
		`SELECT settled FROM pop_ordinal WHERE target_key = ?`, targetKey).Scan(&settled)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read pop ordinal: %w", err)
	}
	return settled, nil
}

// Advance records a settled pop.
func (s SQLOrdinals) Advance(ctx context.Context, targetKey string, to int) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO pop_ordinal (target_key, settled) VALUES (?, ?)
		 ON CONFLICT(target_key) DO UPDATE SET settled = excluded.settled`,
		targetKey, to)
	if err != nil {
		return fmt.Errorf("advance pop ordinal: %w", err)
	}
	return nil
}

// CursorMigration records how far a feed consumer has read.
const CursorMigration = `
CREATE TABLE feed_cursor (
    name   TEXT PRIMARY KEY,
    cursor TEXT NOT NULL
)`

// SQLCursors persists feed cursors in SQLite.
type SQLCursors struct{ DB *sql.DB }

// Current reads a consumer's position. No row is the start of the feed, which
// is where a consumer that has read nothing genuinely is.
func (s SQLCursors) Current(ctx context.Context, name string) (string, error) {
	var cursor string
	err := s.DB.QueryRowContext(ctx,
		`SELECT cursor FROM feed_cursor WHERE name = ?`, name).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read feed cursor: %w", err)
	}
	return cursor, nil
}

// Advance records a consumer's new position.
func (s SQLCursors) Advance(ctx context.Context, name, cursor string) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO feed_cursor (name, cursor) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET cursor = excluded.cursor`, name, cursor)
	if err != nil {
		return fmt.Errorf("advance feed cursor: %w", err)
	}
	return nil
}

// ForTicket returns the unsettled run for an issue, if there is one.
//
// A pop can hand back a ticket a run already exists for — after a rework, or
// after a crash that left the claim standing — and starting a second run for
// it would abandon the first with its review, its attempt count and its
// history. At most one run per issue is unsettled; a closed one is history and
// a new claim on the same issue is genuinely new work.
//
// By ISSUE, never by title. Titles are not unique: a decomposition can produce
// "add tests" twice, and keying on it makes the second claim drive and submit
// the first issue'"'"'s work while orphaning the issue it actually claimed.
func (s SQLStore) ForTicket(ctx context.Context, issue string) (BuildRun, bool, error) {
	var id string
	err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM build_run
		   WHERE issue = ? AND state NOT IN (?, ?, ?)
		   ORDER BY rowid DESC LIMIT 1`,
		issue, string(StateClosed), string(StateMerged), string(StateCancelled)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return BuildRun{}, false, nil
	}
	if err != nil {
		return BuildRun{}, false, fmt.Errorf("find run for ticket: %w", err)
	}
	return s.Find(ctx, id)
}

// ByReview returns the run whose review this is.
//
// For verdicts that carry no session: a spike's finding review has none —
// there was no pair loop and no agent instance to route feedback back to — so
// the review id is the only handle its verdict has.
func (s SQLStore) ByReview(ctx context.Context, review string) (BuildRun, bool, error) {
	var id string
	err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM build_run WHERE review_id = ?`, review).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return BuildRun{}, false, nil
	}
	if err != nil {
		return BuildRun{}, false, fmt.Errorf("find run by review: %w", err)
	}
	return s.Find(ctx, id)
}

// BySession returns the run whose review was stamped with a session.
func (s SQLStore) BySession(ctx context.Context, session string) (BuildRun, bool, error) {
	var id string
	err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM build_run WHERE review_session = ?`, session).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return BuildRun{}, false, nil
	}
	if err != nil {
		return BuildRun{}, false, fmt.Errorf("find run by session: %w", err)
	}
	return s.Find(ctx, id)
}
