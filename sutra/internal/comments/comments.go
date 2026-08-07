// Package comments — see MOD-issues/ENT-comment in avspec.yaml.
// Comments are polymorphic and threaded: each anchors to exactly one
// of issue, doc version, or review (AC-comment-create), and a reply
// nests under its parent with unbounded depth (AC-comment-threading).
// Storage only: anchor-target existence and revision guards live with
// the API layer, which sees every module.
package comments

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"time"
)

// tsLayout is RFC 3339 with FIXED-WIDTH nanoseconds. time.RFC3339Nano
// trims trailing zeros, so its output does not sort lexically: with
// "…992647Z" against "…9926475Z", 'Z' > '5' and the earlier instant
// compares greater. Timestamps are stored and ordered as TEXT, so the
// format IS the ordering (review 1898).
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Comment mirrors the contract's Comment schema.
type Comment struct {
	ID             string  `json:"id"`
	Issue          *string `json:"issue,omitempty"`
	DocVersion     *string `json:"doc_version,omitempty"`
	Review         *string `json:"review,omitempty"`
	ReviewRevision *int64  `json:"review_revision,omitempty"`
	Parent         *string `json:"parent,omitempty"`
	Anchor         *string `json:"anchor,omitempty"`
	Author         string  `json:"author"`
	Body           string  `json:"body"`
	Created        string  `json:"created"`
}

// NotFoundError reports a missing comment (a bad parent reference).
type NotFoundError struct{ ID string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("comment %s not found", e.ID) }

// ParentAnchorError reports a reply whose parent anchors elsewhere —
// a thread never spans anchors.
type ParentAnchorError struct{ Parent string }

func (e *ParentAnchorError) Error() string {
	return fmt.Sprintf("parent comment %s anchors to a different target", e.Parent)
}

// Migrate creates the comments table. The CHECK pins exactly one
// anchor and ties review_revision to review anchors, mirroring the
// contract's oneOf.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS comments (
			id              TEXT PRIMARY KEY,
			issue           TEXT,
			doc_version     TEXT,
			review          TEXT,
			review_revision INTEGER,
			parent          TEXT REFERENCES comments(id),
			anchor          TEXT,
			author          TEXT NOT NULL,
			body            TEXT NOT NULL,
			created         TEXT NOT NULL,
			CHECK ((issue IS NOT NULL) + (doc_version IS NOT NULL) + (review IS NOT NULL) = 1),
			CHECK ((review_revision IS NOT NULL) = (review IS NOT NULL))
		)`)
	if err != nil {
		return fmt.Errorf("migrate comments: %w", err)
	}
	return nil
}

// New is the validated input to Create; exactly one of Issue,
// DocVersion, or Review is set (the caller enforced the oneOf).
type New struct {
	Issue          *string
	DocVersion     *string
	Review         *string
	ReviewRevision *int64
	Parent         *string
	Anchor         *string
	Author         string
	Body           string
}

// Create inserts a comment. A reply's parent must exist and share the
// anchor target — depth itself is unbounded (AC-comment-threading).
func Create(tx *sql.Tx, n New) (Comment, error) {
	if n.Parent != nil {
		parent, err := Get(tx, *n.Parent)
		if err != nil {
			return Comment{}, err
		}
		if !sameAnchor(parent.Issue, n.Issue) || !sameAnchor(parent.DocVersion, n.DocVersion) || !sameAnchor(parent.Review, n.Review) ||
			!sameRevision(parent.ReviewRevision, n.ReviewRevision) {
			// Review revisions are immutable; a thread never spans them.
			return Comment{}, &ParentAnchorError{Parent: *n.Parent}
		}
	}
	c := Comment{
		ID: newUUIDv7(), Issue: n.Issue, DocVersion: n.DocVersion, Review: n.Review,
		ReviewRevision: n.ReviewRevision, Parent: n.Parent, Anchor: n.Anchor,
		Author: n.Author, Body: n.Body,
		Created: time.Now().UTC().Format(tsLayout),
	}
	_, err := tx.Exec(`
		INSERT INTO comments (id, issue, doc_version, review, review_revision, parent, anchor, author, body, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Issue, c.DocVersion, c.Review, c.ReviewRevision, c.Parent, c.Anchor, c.Author, c.Body, c.Created)
	if err != nil {
		return Comment{}, fmt.Errorf("insert comment: %w", err)
	}
	return c, nil
}

func sameRevision(a, b *int64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func sameAnchor(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

// Get returns one comment.
func Get(tx *sql.Tx, id string) (Comment, error) {
	c, err := scanComment(tx.QueryRow(`
		SELECT id, issue, doc_version, review, review_revision, parent, anchor, author, body, created
		FROM comments WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Comment{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Comment{}, fmt.Errorf("get comment %s: %w", id, err)
	}
	return c, nil
}

// ListByAnchor returns the comments on one anchor target in creation
// order; threading reconstructs client-side via parent.
func ListByAnchor(tx *sql.Tx, column, id string) ([]Comment, error) {
	switch column {
	case "issue", "doc_version", "review":
	default:
		return nil, fmt.Errorf("unknown anchor column %q", column)
	}
	rows, err := tx.Query(`
		SELECT id, issue, doc_version, review, review_revision, parent, anchor, author, body, created
		FROM comments WHERE `+column+` = ? ORDER BY created, id`, id)
	if err != nil {
		return nil, fmt.Errorf("list comments by %s: %w", column, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Comment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EachByAnchor streams the comments on one anchor target in creation
// order — comment bodies carry no cap (AC-comment-no-cap), so export
// never accumulates them.
func EachByAnchor(tx *sql.Tx, column, id string, fn func(Comment) error) error {
	switch column {
	case "issue", "doc_version", "review":
	default:
		return fmt.Errorf("unknown anchor column %q", column)
	}
	rows, err := tx.Query(`
		SELECT id, issue, doc_version, review, review_revision, parent, anchor, author, body, created
		FROM comments WHERE `+column+` = ? ORDER BY created, id`, id)
	if err != nil {
		return fmt.Errorf("stream comments by %s: %w", column, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return fmt.Errorf("scan comment: %w", err)
		}
		if err := fn(c); err != nil {
			return err
		}
	}
	return rows.Err()
}

type scannable interface{ Scan(dest ...any) error }

func scanComment(row scannable) (Comment, error) {
	var c Comment
	err := row.Scan(&c.ID, &c.Issue, &c.DocVersion, &c.Review, &c.ReviewRevision,
		&c.Parent, &c.Anchor, &c.Author, &c.Body, &c.Created)
	return c, err
}

// newUUIDv7 returns an RFC 9562 UUIDv7 (CON-uuid-keys); duplicated
// per-package to keep module boundaries dependency-free.
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
