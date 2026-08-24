package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	review_key, review_commit, review_session, review_state, review_id,
	review_revision, review_verdict_event, close_key, completed_head, completion_state`

// SQLStore stores build runs in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert writes a run.
func (s SQLStore) Upsert(ctx context.Context, r BuildRun) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO build_run (id, ticket, issue, branch, plan, state, head, gated_base, error, attempt,
		   round_limit, review_key, review_commit, review_session, review_state, review_id,
		   review_revision, review_verdict_event, close_key, completed_head, completion_state)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   ticket = excluded.ticket, issue = excluded.issue, branch = excluded.branch,
		   plan = excluded.plan, state = excluded.state,
		   head = excluded.head, gated_base = excluded.gated_base, error = excluded.error,
		   attempt = excluded.attempt, round_limit = excluded.round_limit,
		   review_key = excluded.review_key, review_commit = excluded.review_commit,
		   review_session = excluded.review_session, review_state = excluded.review_state,
		   review_id = excluded.review_id,
		   review_revision = excluded.review_revision,
		   review_verdict_event = excluded.review_verdict_event,
		   close_key = excluded.close_key,
		   completed_head = excluded.completed_head,
		   completion_state = excluded.completion_state`,
		r.ID, r.Ticket, r.Issue, r.Branch, r.Plan, string(r.State), r.Head, r.GatedBase, r.Error, r.Attempt,
		r.RoundLimit, r.ReviewKey, r.ReviewCommit, r.ReviewSession,
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
	var state string
	err := s.DB.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM build_run WHERE id = ?`, id).
		Scan(&r.Ticket, &r.Issue, &r.Branch, &r.Plan, &state, &r.Head, &r.GatedBase, &r.Error, &r.Attempt, &r.RoundLimit,
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
	return r, true, nil
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
	defer func() { _ = rows.Close() }()

	var out []BuildRun
	for rows.Next() {
		var r BuildRun
		var runState string
		if err := rows.Scan(&r.ID, &r.Ticket, &r.Issue, &r.Branch, &r.Plan, &runState, &r.Head, &r.GatedBase, &r.Error,
			&r.Attempt, &r.RoundLimit, &r.ReviewKey, &r.ReviewCommit,
			&r.ReviewSession, &r.ReviewState, &r.ReviewID,
			&r.ReviewRevision, &r.ReviewVerdictEvent,
			&r.CloseKey, &r.CompletedHead, &r.CompletionState); err != nil {
			return nil, fmt.Errorf("scan %s run: %w", state, err)
		}
		r.State = State(runState)
		out = append(out, r)
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list that reads exactly like "every review reached sutra".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s runs: %w", state, err)
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
