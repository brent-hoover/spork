// Package threads — see MOD-threads in avspec.yaml for responsibility
// and boundaries. A thread is an imported agent transcript preserved
// verbatim (AC-thread-import), anchored from import onward to a project
// or an issue — at least one, retargetable later (AC-thread-anchor).
package threads

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// tsLayout is RFC 3339 with FIXED-WIDTH nanoseconds. time.RFC3339Nano
// trims trailing zeros, so its output does not sort lexically: with
// "…992647Z" against "…9926475Z", 'Z' > '5' and the earlier instant
// compares greater. Timestamps are stored and ordered as TEXT, so the
// format IS the ordering (review 1898).
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Thread mirrors the contract's Thread schema. Transcript is raw JSON
// carried untouched from import to serving, so the stored content
// matches the imported file byte-for-byte.
type Thread struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Transcript json.RawMessage `json:"transcript"`
	Session    *string         `json:"session"`
	Project    *string         `json:"project,omitempty"`
	Issue      *string         `json:"issue,omitempty"`
	ImportedAt string          `json:"imported_at"`
}

// Anchor is the (project, issue) pair a thread ties to; at least one
// side is always set — the table CHECK is the authority.
type Anchor struct {
	Project *string `json:"project"`
	Issue   *string `json:"issue"`
}

// NotFoundError reports a missing thread.
type NotFoundError struct{ ID string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("thread %s not found", e.ID) }

// Migrate creates the threads table.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS threads (
			id          TEXT PRIMARY KEY,
			title       TEXT NOT NULL,
			transcript  TEXT NOT NULL,
			session     TEXT,
			project     TEXT REFERENCES projects(id),
			issue       TEXT REFERENCES issues(id),
			imported_at TEXT NOT NULL,
			CHECK (project IS NOT NULL OR issue IS NOT NULL)
		)`)
	if err != nil {
		return fmt.Errorf("migrate threads: %w", err)
	}
	return nil
}

// Create imports a transcript with its anchor already validated by the
// caller (existence, writability, at-least-one-side).
func Create(tx *sql.Tx, title string, transcript json.RawMessage, session *string, anchor Anchor) (Thread, error) {
	t := Thread{
		ID:         newUUIDv7(),
		Title:      title,
		Transcript: transcript,
		Session:    session,
		Project:    anchor.Project,
		Issue:      anchor.Issue,
		ImportedAt: time.Now().UTC().Format(tsLayout),
	}
	_, err := tx.Exec(`
		INSERT INTO threads (id, title, transcript, session, project, issue, imported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, string(t.Transcript), t.Session, t.Project, t.Issue, t.ImportedAt)
	if err != nil {
		return Thread{}, fmt.Errorf("insert thread %q: %w", title, err)
	}
	return t, nil
}

// Get returns one thread.
func Get(tx *sql.Tx, id string) (Thread, error) {
	row := tx.QueryRow(`
		SELECT id, title, transcript, session, project, issue, imported_at
		FROM threads WHERE id = ?`, id)
	t, err := scanThread(row)
	if err == sql.ErrNoRows {
		return Thread{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Thread{}, fmt.Errorf("get thread %s: %w", id, err)
	}
	return t, nil
}

// SetAnchor replaces the thread's anchor entirely — omitted sides
// clear — and returns the old anchor alongside the updated thread so
// the caller can carry both in the anchor-changed event payload.
func SetAnchor(tx *sql.Tx, id string, anchor Anchor) (Thread, Anchor, error) {
	current, err := Get(tx, id)
	if err != nil {
		return Thread{}, Anchor{}, err
	}
	old := Anchor{Project: current.Project, Issue: current.Issue}
	if _, err := tx.Exec(`UPDATE threads SET project = ?, issue = ? WHERE id = ?`,
		anchor.Project, anchor.Issue, id); err != nil {
		return Thread{}, Anchor{}, fmt.Errorf("set anchor of thread %s: %w", id, err)
	}
	current.Project = anchor.Project
	current.Issue = anchor.Issue
	return current, old, nil
}

// SearchEach streams threads matching substring q on title or
// transcript (AC-thread-search), session (AC-search-session), and
// anchored project (the project side of AC-thread-anchor's listing) —
// every filter optional — one row at a time to fn. Transcripts can
// approach SQLite's value bound, so listings never accumulate them:
// memory stays at one thread regardless of catalog size.
func SearchEach(tx *sql.Tx, q, session, project *string, fn func(Thread) error) error {
	where := []string{"1=1"}
	var args []any
	if q != nil {
		where = append(where, `(title LIKE '%' || ? || '%' ESCAPE '\' OR transcript LIKE '%' || ? || '%' ESCAPE '\')`)
		escaped := escapeLike(*q)
		args = append(args, escaped, escaped)
	}
	if session != nil {
		where = append(where, "session = ?")
		args = append(args, *session)
	}
	if project != nil {
		// A thread anchored solely to an issue still belongs to that
		// issue's project — the project scope covers both anchor sides.
		where = append(where, "(project = ? OR issue IN (SELECT id FROM issues WHERE project = ?))")
		args = append(args, *project, *project)
	}
	return queryThreadsEach(tx, `
		SELECT id, title, transcript, session, project, issue, imported_at
		FROM threads WHERE `+strings.Join(where, " AND ")+` ORDER BY imported_at, id`, fn, args...)
}

// ListByIssueEach streams the threads anchored to an issue — the
// issue side of AC-thread-anchor's "both sides list it".
func ListByIssueEach(tx *sql.Tx, issueID string, fn func(Thread) error) error {
	return queryThreadsEach(tx, `
		SELECT id, title, transcript, session, project, issue, imported_at
		FROM threads WHERE issue = ? ORDER BY imported_at, id`, fn, issueID)
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func queryThreadsEach(tx *sql.Tx, query string, fn func(Thread) error, args ...any) error {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return fmt.Errorf("query threads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return fmt.Errorf("scan thread: %w", err)
		}
		if err := fn(t); err != nil {
			return err
		}
	}
	return rows.Err()
}

type scannable interface{ Scan(dest ...any) error }

func scanThread(row scannable) (Thread, error) {
	var t Thread
	var transcript string
	if err := row.Scan(&t.ID, &t.Title, &transcript, &t.Session, &t.Project, &t.Issue, &t.ImportedAt); err != nil {
		return Thread{}, err
	}
	t.Transcript = json.RawMessage(transcript)
	return t, nil
}

// newUUIDv7 returns an RFC 9562 UUIDv7 — time-ordered primary keys per
// CON-uuid-keys. Hand-rolled to keep the dependency surface at the
// approved set.
func newUUIDv7() string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixMilli())<<16) //nolint:gosec // UnixMilli is non-negative for all realistic clocks
	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
