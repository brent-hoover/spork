package devloop

import (
	"context"
	"database/sql"
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

// SQLStore stores sessions in SQLite.
type SQLStore struct{ DB *sql.DB }

// Upsert writes a session row.
func (s SQLStore) Upsert(ctx context.Context, sess Session) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO dev_session (run, ticket, session_id, model, ended)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(run) DO UPDATE SET
		   ticket = excluded.ticket, session_id = excluded.session_id,
		   model = excluded.model, ended = excluded.ended`,
		sess.Run, sess.Ticket, sess.SessionID, sess.Model, sess.Ended)
	if err != nil {
		return fmt.Errorf("upsert dev session: %w", err)
	}
	return nil
}
