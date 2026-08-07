// Package projects — see MOD-projects in avspec.yaml. Repo-anchored
// containers for issues and docs: created by `sutra init` through the
// public API, archived to hide without deleting (AC-project-archive).
package projects

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
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

// Project mirrors the contract's Project schema. Optional fields are
// pointers so absent stays absent on the wire.
type Project struct {
	ID            string  `json:"id"`
	Key           string  `json:"key"`
	Name          string  `json:"name"`
	Description   *string `json:"description,omitempty"`
	RepoPath      *string `json:"repo_path,omitempty"`
	DefaultBranch *string `json:"default_branch,omitempty"`
	ArchivedAt    *string `json:"archived_at,omitempty"`
}

// New is the create request's domain shape.
type New struct {
	Key           string
	Name          string
	Description   *string
	RepoPath      *string
	DefaultBranch *string
}

// DuplicateKeyError reports a unique-violation on the project key,
// naming the colliding project (AC-unique-violation).
type DuplicateKeyError struct {
	Key        string
	ExistingID string
}

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("key %q already names project %s", e.Key, e.ExistingID)
}

// AlreadyArchivedError reports an archive of an archived project.
type AlreadyArchivedError struct{ ID string }

func (e *AlreadyArchivedError) Error() string {
	return fmt.Sprintf("project %s is already archived", e.ID)
}

// NotFoundError reports an id that names no project.
type NotFoundError struct{ ID string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("project %s not found", e.ID) }

// Migrate creates the projects table.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS projects (
			id             TEXT PRIMARY KEY,
			key            TEXT NOT NULL UNIQUE,
			name           TEXT NOT NULL,
			description    TEXT,
			repo_path      TEXT,
			default_branch TEXT NOT NULL DEFAULT 'main',
			archived_at    TEXT
		)`)
	if err != nil {
		return fmt.Errorf("migrate projects: %w", err)
	}
	return nil
}

// Create inserts a project. The UNIQUE constraint on key is the
// authority on duplicates: concurrent creates race safely, losers get
// *DuplicateKeyError naming the winner. default_branch defaults to
// "main" when omitted, per the contract.
func Create(tx *sql.Tx, n New) (Project, error) {
	id := newUUIDv7()
	branch := "main"
	if n.DefaultBranch != nil {
		branch = *n.DefaultBranch
	}
	_, err := tx.Exec(`
		INSERT INTO projects (id, key, name, description, repo_path, default_branch)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, n.Key, n.Name, n.Description, n.RepoPath, branch)
	if err == nil {
		return Project{ID: id, Key: n.Key, Name: n.Name, Description: n.Description,
			RepoPath: n.RepoPath, DefaultBranch: &branch}, nil
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return Project{}, fmt.Errorf("insert project %q: %w", n.Key, err)
	}
	var existing string
	if scanErr := tx.QueryRow(`SELECT id FROM projects WHERE key = ?`, n.Key).Scan(&existing); scanErr != nil {
		return Project{}, fmt.Errorf("resolve duplicate key %q: %w", n.Key, scanErr)
	}
	return Project{}, &DuplicateKeyError{Key: n.Key, ExistingID: existing}
}

// Get returns one project by id.
func Get(db *sql.DB, id string) (Project, error) {
	p, err := scanOne(db.QueryRow(`
		SELECT id, key, name, description, repo_path, default_branch, archived_at
		FROM projects WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Project{}, &NotFoundError{ID: id}
	}
	return p, err
}

// GetTx returns one project inside the caller's transaction.
func GetTx(tx *sql.Tx, id string) (Project, error) {
	p, err := scanOne(tx.QueryRow(`
		SELECT id, key, name, description, repo_path, default_branch, archived_at
		FROM projects WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Project{}, &NotFoundError{ID: id}
	}
	return p, err
}

// List returns projects; archived ones only when includeArchived
// (AC-project-archive: hidden from default listings).
// Cursor streams projects from an opened query. Opening is separate
// from iterating so a caller can commit an HTTP status only once the
// read has started, and names and descriptions — unconstrained by the
// contract — are never accumulated (review 1922).
type Cursor struct{ rows *sql.Rows }

// OpenList runs the listing query ordered by key.
func OpenList(db *sql.DB, includeArchived bool) (*Cursor, error) {
	query := `SELECT id, key, name, description, repo_path, default_branch, archived_at FROM projects`
	if !includeArchived {
		query += ` WHERE archived_at IS NULL`
	}
	query += ` ORDER BY key`
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return &Cursor{rows: rows}, nil
}

func (c *Cursor) Each(fn func(Project) error) error {
	for c.rows.Next() {
		p, err := scanOne(c.rows)
		if err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	if err := c.rows.Err(); err != nil {
		return fmt.Errorf("iterate projects: %w", err)
	}
	return nil
}

func (c *Cursor) Close() error { return c.rows.Close() }

func List(db *sql.DB, includeArchived bool) ([]Project, error) {
	query := `SELECT id, key, name, description, repo_path, default_branch, archived_at FROM projects`
	if !includeArchived {
		query += ` WHERE archived_at IS NULL`
	}
	query += ` ORDER BY key`
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Project{}
	for rows.Next() {
		p, err := scanOne(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}
	return out, nil
}

// Archive stamps archived_at exactly once. The conditional UPDATE is the
// race authority: a second archive — concurrent or later — hits zero
// rows and resolves to AlreadyArchivedError or NotFoundError.
func Archive(tx *sql.Tx, id string) (Project, error) {
	res, err := tx.Exec(`UPDATE projects SET archived_at = ? WHERE id = ? AND archived_at IS NULL`,
		time.Now().UTC().Format(tsLayout), id)
	if err != nil {
		return Project{}, fmt.Errorf("archive project %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Project{}, fmt.Errorf("archive project %s: %w", id, err)
	}
	if n == 0 {
		var one int
		switch scanErr := tx.QueryRow(`SELECT 1 FROM projects WHERE id = ?`, id).Scan(&one); scanErr {
		case nil:
			return Project{}, &AlreadyArchivedError{ID: id}
		case sql.ErrNoRows:
			return Project{}, &NotFoundError{ID: id}
		default:
			return Project{}, fmt.Errorf("resolve archive of %s: %w", id, scanErr)
		}
	}
	p, err := scanOne(tx.QueryRow(`
		SELECT id, key, name, description, repo_path, default_branch, archived_at
		FROM projects WHERE id = ?`, id))
	if err != nil {
		return Project{}, fmt.Errorf("read archived project %s: %w", id, err)
	}
	return p, nil
}

// IsArchived reports whether id names an archived project — the write
// guard every project-scoped mutation consults (writes to an archived
// project are rejected; reads still work).
func IsArchived(tx *sql.Tx, id string) (bool, error) {
	var archived sql.NullString
	err := tx.QueryRow(`SELECT archived_at FROM projects WHERE id = ?`, id).Scan(&archived)
	switch {
	case err == sql.ErrNoRows:
		return false, &NotFoundError{ID: id}
	case err != nil:
		return false, fmt.Errorf("check archive of %s: %w", id, err)
	}
	return archived.Valid, nil
}

type rowScanner interface{ Scan(...any) error }

func scanOne(r rowScanner) (Project, error) {
	var p Project
	if err := r.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &p.RepoPath, &p.DefaultBranch, &p.ArchivedAt); err != nil {
		return Project{}, err
	}
	return p, nil
}

// newUUIDv7 returns an RFC 9562 UUIDv7 (CON-uuid-keys). Duplicated
// per-package because MOD-projects imports no sibling modules.
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
