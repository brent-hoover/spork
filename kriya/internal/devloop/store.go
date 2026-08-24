package devloop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Migration is dev-loop's schema.
const Migration = `
CREATE TABLE dev_session (
    run        TEXT PRIMARY KEY,
    ticket     TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    model      TEXT NOT NULL DEFAULT '',
    ended      INTEGER NOT NULL DEFAULT 0
)`

// CaptureMigration adds the transcript import's write-ahead columns.
//
// A separate migration for the same reason as the last: the ledger records
// which ran, and rewriting an applied one leaves existing databases claiming
// columns they do not have.
const CaptureMigration = `
ALTER TABLE dev_session ADD COLUMN transcript_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE dev_session ADD COLUMN import_key TEXT NOT NULL DEFAULT '';
ALTER TABLE dev_session ADD COLUMN import_state TEXT NOT NULL DEFAULT 'none';
ALTER TABLE dev_session ADD COLUMN thread_ref TEXT NOT NULL DEFAULT ''`

// RoundsMigration adds what the pair loop produces.
//
// A second migration rather than an edit to the first: the ledger records
// which migrations ran, and rewriting an applied one leaves every existing
// database claiming to have columns it does not have.
const RoundsMigration = `
ALTER TABLE dev_session ADD COLUMN rounds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dev_session ADD COLUMN commits TEXT NOT NULL DEFAULT '[]'`

// SystemFileMigration records where the assembled context was written.
//
// A resumed pass needs it: the fix rounds append the SAME bundle the first
// round read, and a session recovered without it would hand the agent no law
// at all. It was only ever in memory, which is exactly as long as a pass that
// comes back does not last.
const SystemFileMigration = `
ALTER TABLE dev_session ADD COLUMN system_file TEXT NOT NULL DEFAULT ''`

// ResumeMigration records what a pass that comes back resumes from.
//
// The conversation and the round still being waited on. Held only in memory,
// a resumed pass started a fresh conversation and re-submitted under a round
// id whose job was already running.
const ResumeMigration = `
ALTER TABLE dev_session ADD COLUMN turns TEXT NOT NULL DEFAULT '[]';
ALTER TABLE dev_session ADD COLUMN pending_round TEXT NOT NULL DEFAULT '{}'`

// sessionColumns is every column a Session reads back, in scan order.
const sessionColumns = `ticket, session_id, model, ended, rounds, commits,
	system_file, transcript_ref, import_key, import_state, thread_ref,
	turns, pending_round`

// SQLStore stores sessions in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert writes a session row.
func (s SQLStore) Upsert(ctx context.Context, sess Session) error {
	commits, err := json.Marshal(sess.Commits)
	if err != nil {
		return fmt.Errorf("encode commits: %w", err)
	}
	turns, err := json.Marshal(sess.Turns)
	if err != nil {
		return fmt.Errorf("encode turns: %w", err)
	}
	pending, err := json.Marshal(sess.Pending)
	if err != nil {
		return fmt.Errorf("encode pending round: %w", err)
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO dev_session
		   (run, ticket, session_id, model, ended, rounds, commits,
		    system_file, transcript_ref, import_key, import_state, thread_ref,
		    turns, pending_round)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run) DO UPDATE SET
		   ticket = excluded.ticket, session_id = excluded.session_id,
		   model = excluded.model, ended = excluded.ended,
		   rounds = excluded.rounds, commits = excluded.commits,
		   system_file = excluded.system_file,
		   transcript_ref = excluded.transcript_ref,
		   import_key = excluded.import_key,
		   import_state = excluded.import_state,
		   thread_ref = excluded.thread_ref,
		   turns = excluded.turns, pending_round = excluded.pending_round`,
		sess.Run, sess.Ticket, sess.SessionID, sess.Model, sess.Ended,
		sess.Rounds, string(commits), sess.SystemFile, sess.TranscriptRef, sess.ImportKey,
		importStateOf(sess), sess.ThreadRef, string(turns), string(pending))
	if err != nil {
		return fmt.Errorf("upsert dev session: %w", err)
	}
	return nil
}

// Find reads a run's session.
func (s SQLStore) Find(ctx context.Context, run string) (Session, bool, error) {
	sess := Session{Run: run}
	var commits, turns, pending string
	err := s.DB.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM dev_session WHERE run = ?`, run).
		Scan(&sess.Ticket, &sess.SessionID, &sess.Model, &sess.Ended, &sess.Rounds,
			&commits, &sess.SystemFile, &sess.TranscriptRef, &sess.ImportKey,
			&sess.ImportState, &sess.ThreadRef, &turns, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("read dev session: %w", err)
	}
	if err := json.Unmarshal([]byte(commits), &sess.Commits); err != nil {
		return Session{}, false, fmt.Errorf("decode commits: %w", err)
	}
	if err := json.Unmarshal([]byte(turns), &sess.Turns); err != nil {
		return Session{}, false, fmt.Errorf("decode turns: %w", err)
	}
	if err := json.Unmarshal([]byte(pending), &sess.Pending); err != nil {
		return Session{}, false, fmt.Errorf("decode pending round: %w", err)
	}
	return sess, true, nil
}

// importStateOf defaults an unset state.
//
// A zero-valued Session has no import state, and writing "" would put a value
// outside the declared enum in the column.
func importStateOf(sess Session) string {
	if sess.ImportState == "" {
		return ImportNone
	}
	return sess.ImportState
}

// Importing lists sessions whose transcript import a crash left in flight.
func (s SQLStore) Importing(ctx context.Context) ([]Session, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT run, ticket, session_id, model, transcript_ref, import_key
		   FROM dev_session WHERE import_state = ? ORDER BY run`, ImportImporting)
	if err != nil {
		return nil, fmt.Errorf("query importing sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Session
	for rows.Next() {
		sess := Session{ImportState: ImportImporting}
		if err := rows.Scan(&sess.Run, &sess.Ticket, &sess.SessionID, &sess.Model,
			&sess.TranscriptRef, &sess.ImportKey); err != nil {
			return nil, fmt.Errorf("scan importing session: %w", err)
		}
		out = append(out, sess)
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list that reads exactly like "every transcript reached the
	// catalog".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate importing sessions: %w", err)
	}
	return out, nil
}
