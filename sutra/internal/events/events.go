// Package events — see MOD-events in avspec.yaml. One append-only
// stream, consumed twice: the pollable feed and per-issue audit history
// (AC-audit-immutable). Nothing here updates or deletes; the table's
// triggers make mutation impossible even for future callers.
package events

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// tsLayout is RFC 3339 with FIXED-WIDTH nanoseconds. time.RFC3339Nano
// trims trailing zeros, so its output does not sort lexically: with
// "…992647Z" against "…9926475Z", 'Z' > '5' and the earlier instant
// compares greater. Timestamps are stored and ordered as TEXT, so the
// format IS the ordering (review 1898).
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Event is one feed entry, shaped as the contract's EventBase.
type Event struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Subject   string          `json:"subject"`
	Operation string          `json:"operation"`
	Actor     string          `json:"actor"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Created   string          `json:"created"`
	seq       int64           // feed position; exposed only through cursors
}

// Page is the contract's EventPage: a cursor-bounded slice of the feed.
type Page struct {
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor"`
	Drained    bool    `json:"drained"`
}

// Migrate creates the events table. UPDATE and DELETE are blocked by
// triggers so append-only holds at the store, not by convention.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS events (
			seq       INTEGER PRIMARY KEY AUTOINCREMENT,
			id        TEXT NOT NULL UNIQUE,
			kind      TEXT NOT NULL,
			subject   TEXT NOT NULL,
			operation TEXT NOT NULL,
			actor     TEXT NOT NULL,
			payload   TEXT,
			created   TEXT NOT NULL
		);
		-- Subject-scoped reads (audit, project export) walk one subject's
		-- slice of the feed in feed order; without this they scan the
		-- whole table (review 1898).
		CREATE INDEX IF NOT EXISTS events_subject_seq ON events(subject, seq);
		CREATE TRIGGER IF NOT EXISTS events_append_only_update
			BEFORE UPDATE ON events
			BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
		CREATE TRIGGER IF NOT EXISTS events_append_only_delete
			BEFORE DELETE ON events
			BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
	`)
	if err != nil {
		return fmt.Errorf("migrate events: %w", err)
	}
	return nil
}

// NewOperation mints the server-generated transaction id every event of
// one mutating transaction shares — deliberately not the client's
// Idempotency-Key, which is only request-scoped.
func NewOperation() string { return newUUIDv7() }

// Emit appends one event inside the mutating transaction, so the event
// is exactly as durable as the change it records. It returns the event
// id — verdict machinery stores it as latest_verdict_event.
func Emit(tx *sql.Tx, kind, subject, operation, actor string, payload *string) (string, error) {
	id := newUUIDv7()
	_, err := tx.Exec(`
		INSERT INTO events (id, kind, subject, operation, actor, payload, created)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, kind, subject, operation, actor, payload,
		time.Now().UTC().Format(tsLayout))
	if err != nil {
		return "", fmt.Errorf("emit %s for %s: %w", kind, subject, err)
	}
	return id, nil
}

// Watermark captures the feed position atomically within the caller's
// transaction: processing the feed through it provably covers every
// event that could have affected state read in the same transaction
// (AC-feed-watermark).
func Watermark(tx *sql.Tx) (string, error) {
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&maxSeq); err != nil {
		return "", fmt.Errorf("capture watermark: %w", err)
	}
	return strconv.FormatInt(maxSeq.Int64, 10), nil
}

// List returns one feed page after cursor, optionally filtered by kind
// and subject. With until set, Drained reports whether every matching
// event through that fixed position has now been returned — later
// concurrent appends never move the bound (AC-feed-drain). Without
// until, Drained is always false, never omitted.
func List(db *sql.DB, cursor, kind, subject string, limit int, until string) (Page, error) {
	return list(db, cursor, kind, subject, limit, until, nil)
}

// ListStream is List with per-row delivery: fn receives each event as
// it scans and the returned Page carries only cursor/drained metadata.
func ListStream(db *sql.DB, cursor, kind, subject string, limit int, until string, fn func(Event) error) (Page, error) {
	return list(db, cursor, kind, subject, limit, until, fn)
}

func list(db *sql.DB, cursor, kind, subject string, limit int, until string, fn func(Event) error) (Page, error) {
	after, err := parseCursor(cursor)
	if err != nil {
		return Page{}, err
	}
	var bound int64
	if until != "" {
		if bound, err = parseCursor(until); err != nil {
			return Page{}, err
		}
	}
	// Positions the feed never issued are rejected, not silently
	// honored: a cursor beyond the head would skip events forever, and
	// a fabricated future until would report drained before events
	// inside its bound exist. The head only grows, so a value valid
	// here can never become invalid mid-drain.
	var head sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&head); err != nil {
		return Page{}, fmt.Errorf("read feed head: %w", err)
	}
	if after > head.Int64 {
		return Page{}, &BadCursorError{Cursor: cursor}
	}
	if until != "" && bound > head.Int64 {
		return Page{}, &BadCursorError{Cursor: until}
	}
	query := `SELECT seq, id, kind, subject, operation, actor, payload, created FROM events WHERE seq > ?`
	args := []any{after}
	if until != "" {
		// The bound constrains the page itself: an event appended past
		// the fixed watermark never enters a drain's pages and never
		// advances its cursor beyond the bound.
		query += ` AND seq <= ?`
		args = append(args, bound)
	}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	if subject != "" {
		query += ` AND subject = ?`
		args = append(args, subject)
	}
	query += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("list events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := Page{Events: []Event{}}
	last := after
	for rows.Next() {
		var e Event
		var payload sql.NullString
		if err := rows.Scan(&e.seq, &e.ID, &e.Kind, &e.Subject, &e.Operation, &e.Actor, &payload, &e.Created); err != nil {
			return Page{}, fmt.Errorf("scan event: %w", err)
		}
		if payload.Valid {
			e.Payload = json.RawMessage(payload.String)
		}
		last = e.seq
		if fn != nil {
			// Streaming consumers take each row and drop it — payloads
			// are unbounded once imports carry them, so a page never
			// accumulates in memory.
			if err := fn(e); err != nil {
				return Page{}, err
			}
			continue
		}
		page.Events = append(page.Events, e)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("iterate events: %w", err)
	}
	page.NextCursor = strconv.FormatInt(last, 10)

	if until != "" {
		remaining := `SELECT COUNT(*) FROM events WHERE seq > ? AND seq <= ?`
		remArgs := []any{last, bound}
		if kind != "" {
			remaining += ` AND kind = ?`
			remArgs = append(remArgs, kind)
		}
		if subject != "" {
			remaining += ` AND subject = ?`
			remArgs = append(remArgs, subject)
		}
		var n int
		if err := db.QueryRow(remaining, remArgs...).Scan(&n); err != nil {
			return Page{}, fmt.Errorf("check drain bound: %w", err)
		}
		page.Drained = n == 0
	}
	return page, nil
}

// BySubjectEach streams a subject's audit history row by row — the
// history is unbounded and payloads can be large once imported.
func BySubjectEach(tx *sql.Tx, subject string, fn func(Event) error) error {
	rows, err := tx.Query(`
		SELECT seq, id, kind, subject, operation, actor, payload, created
		FROM events WHERE subject = ? ORDER BY seq`, subject)
	if err != nil {
		return fmt.Errorf("events of %s: %w", subject, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e Event
		var payload sql.NullString
		if err := rows.Scan(&e.seq, &e.ID, &e.Kind, &e.Subject, &e.Operation, &e.Actor, &payload, &e.Created); err != nil {
			return fmt.Errorf("scan event: %w", err)
		}
		if payload.Valid {
			e.Payload = json.RawMessage(payload.String)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// BadCursorError reports a cursor that never came from this feed.
type BadCursorError struct{ Cursor string }

func (e *BadCursorError) Error() string { return fmt.Sprintf("malformed cursor %q", e.Cursor) }

func parseCursor(c string) (int64, error) {
	if c == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(c, 10, 64)
	if err != nil || n < 0 {
		return 0, &BadCursorError{Cursor: c}
	}
	return n, nil
}

// newUUIDv7 returns an RFC 9562 UUIDv7 (CON-uuid-keys). Duplicated
// per-package because MOD-events imports no sibling modules.
func newUUIDv7() string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixMilli())<<16) //nolint:gosec // UnixMilli is non-negative for all realistic clocks
	// crypto/rand.Read cannot return an error — see the note in
	// internal/identity for the three-way proof (documented contract,
	// fatal() before any non-nil return at crypto/rand/rand.go:63-66, and
	// a failing rand.Reader producing a process fatal rather than an
	// error). The guard that stood here was dead in nine places at once.
	_, _ = rand.Read(b[6:])
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
