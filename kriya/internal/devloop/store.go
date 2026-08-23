package devloop

import (
	"context"
	"database/sql"
	"encoding/json"
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

// SQLStore stores sessions in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert writes a session row.
func (s SQLStore) Upsert(ctx context.Context, sess Session) error {
	commits, err := json.Marshal(sess.Commits)
	if err != nil {
		return fmt.Errorf("encode commits: %w", err)
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO dev_session
		   (run, ticket, session_id, model, ended, rounds, commits,
		    transcript_ref, import_key, import_state, thread_ref)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run) DO UPDATE SET
		   ticket = excluded.ticket, session_id = excluded.session_id,
		   model = excluded.model, ended = excluded.ended,
		   rounds = excluded.rounds, commits = excluded.commits,
		   transcript_ref = excluded.transcript_ref,
		   import_key = excluded.import_key,
		   import_state = excluded.import_state,
		   thread_ref = excluded.thread_ref`,
		sess.Run, sess.Ticket, sess.SessionID, sess.Model, sess.Ended,
		sess.Rounds, string(commits), sess.TranscriptRef, sess.ImportKey,
		importStateOf(sess), sess.ThreadRef)
	if err != nil {
		return fmt.Errorf("upsert dev session: %w", err)
	}
	return nil
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
