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
		`INSERT INTO dev_session (run, ticket, session_id, model, ended, rounds, commits)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run) DO UPDATE SET
		   ticket = excluded.ticket, session_id = excluded.session_id,
		   model = excluded.model, ended = excluded.ended,
		   rounds = excluded.rounds, commits = excluded.commits`,
		sess.Run, sess.Ticket, sess.SessionID, sess.Model, sess.Ended,
		sess.Rounds, string(commits))
	if err != nil {
		return fmt.Errorf("upsert dev session: %w", err)
	}
	return nil
}
