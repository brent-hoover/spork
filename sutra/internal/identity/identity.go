// Package identity — see MOD-identity in avspec.yaml for responsibility
// and boundaries. Identities are plain named kinds: a unique handle and
// human|agent, no credentials (AC-identity-create).
package identity

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// Identity mirrors the contract's Identity schema.
type Identity struct {
	ID          string  `json:"id"`
	Handle      string  `json:"handle"`
	Kind        string  `json:"kind"`
	DisplayName *string `json:"display_name,omitempty"`
}

// DuplicateHandleError reports a unique-violation on handle, naming the
// colliding identity so the API can populate Error.conflicts
// (AC-unique-violation).
type DuplicateHandleError struct {
	Handle     string
	ExistingID string
}

func (e *DuplicateHandleError) Error() string {
	return fmt.Sprintf("handle %q already names identity %s", e.Handle, e.ExistingID)
}

// Migrate creates the identities table.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS identities (
			id           TEXT PRIMARY KEY,
			handle       TEXT NOT NULL UNIQUE,
			kind         TEXT NOT NULL CHECK (kind IN ('human', 'agent')),
			display_name TEXT
		)`)
	if err != nil {
		return fmt.Errorf("migrate identities: %w", err)
	}
	return nil
}

// Create inserts a new identity. The UNIQUE constraint is the authority
// on duplicates — the insert is attempted first, and a constraint
// violation is translated by re-reading the holder, so concurrent
// creates of the same handle race safely: exactly one wins and every
// loser gets *DuplicateHandleError naming the winner.
func Create(tx *sql.Tx, handle, kind string, displayName *string) (Identity, error) {
	id := NewUUIDv7()
	_, err := tx.Exec(`INSERT INTO identities (id, handle, kind, display_name) VALUES (?, ?, ?, ?)`,
		id, handle, kind, displayName)
	if err == nil {
		return Identity{ID: id, Handle: handle, Kind: kind, DisplayName: displayName}, nil
	}
	if !IsUniqueViolation(err) {
		return Identity{}, fmt.Errorf("insert identity %q: %w", handle, err)
	}
	var existing string
	if scanErr := tx.QueryRow(`SELECT id FROM identities WHERE handle = ?`, handle).Scan(&existing); scanErr != nil {
		return Identity{}, fmt.Errorf("resolve duplicate handle %q: %w", handle, scanErr)
	}
	return Identity{}, &DuplicateHandleError{Handle: handle, ExistingID: existing}
}

// IsUniqueViolation reports whether err is a SQLite UNIQUE constraint
// failure. Matched on the stable constraint message because the driver
// wraps its typed error inconsistently across call paths.
func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// NewUUIDv7 returns an RFC 9562 UUIDv7 — time-ordered primary keys per
// CON-uuid-keys. Hand-rolled to keep the dependency surface at the
// spec's pinned set.
func NewUUIDv7() string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixMilli())<<16) //nolint:gosec // UnixMilli is non-negative for all realistic clocks
	// crypto/rand.Read cannot return an error, so there is nothing here to
	// check. Established three ways rather than taken from the doc comment,
	// which is the mistake dupkeys.go cost a round on: the documentation
	// says "It never returns an error"; the implementation routes every
	// failure through fatal() followed by panic("unreachable") BEFORE any
	// non-nil return (crypto/rand/rand.go:63-66); and replacing
	// rand.Reader with a failing one in a test produced a process-level
	// fatal, not a returned error. The guard that used to be here panicked
	// on a condition the standard library already crashes on, with a worse
	// message than its own.
	_, _ = rand.Read(b[6:])
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// List returns identities, optionally filtered by kind.
// Cursor streams identities from an opened query — display names are
// unconstrained, so a listing never accumulates them (review 1922).
type Cursor struct{ rows *sql.Rows }

// OpenList runs the listing query ordered by handle.
func OpenList(db *sql.DB, kind string) (*Cursor, error) {
	query, args := `SELECT id, handle, kind, display_name FROM identities ORDER BY handle`, []any{}
	if kind != "" {
		query, args = `SELECT id, handle, kind, display_name FROM identities WHERE kind = ? ORDER BY handle`, []any{kind}
	}
	rows, err := db.Query(query, args...)
	// The cursor takes the handle BEFORE the error is examined, so every
	// exit from here closes what the query opened. Review 1989 read this
	// as a branch nothing reaches — db.Query does yield nil rows with its
	// error, and Close's nil case is what serves that path (the
	// failed-query test drives it). The shape is not for that path. It is
	// for an exit added here that returns without the handle: a leaked
	// reader does not fail, it starves SQLite's writers, so the symptom
	// is the suite hanging somewhere later. Removing the ownership made
	// the mutation of this very line TIME OUT rather than die — the
	// mutant returned early holding open rows, and the next scenario's
	// write waited forever.
	cursor := &Cursor{rows: rows}
	if err != nil {
		_ = cursor.Close()
		return nil, fmt.Errorf("list identities: %w", err)
	}
	return cursor, nil
}

func (c *Cursor) Each(fn func(Identity) error) error {
	for c.rows.Next() {
		var i Identity
		if err := c.rows.Scan(&i.ID, &i.Handle, &i.Kind, &i.DisplayName); err != nil {
			return fmt.Errorf("scan identity: %w", err)
		}
		if err := fn(i); err != nil {
			return err
		}
	}
	if err := c.rows.Err(); err != nil {
		return fmt.Errorf("iterate identities: %w", err)
	}
	return nil
}

// Close releases the cursor's rows, returning its connection to the pool;
// a listing abandoned mid-stream would otherwise hold one. A cursor whose
// query failed holds no rows — OpenList closes one of those on every
// failed open — so the nil case is a live path, not a defensive one.
func (c *Cursor) Close() error {
	if c.rows == nil {
		return nil
	}
	return c.rows.Close()
}

// Lookup returns the identity an id names, or NotFoundError.
func Lookup(tx *sql.Tx, id string) (Identity, error) {
	var i Identity
	err := tx.QueryRow(`SELECT id, handle, kind, display_name FROM identities WHERE id = ?`, id).
		Scan(&i.ID, &i.Handle, &i.Kind, &i.DisplayName)
	if err == sql.ErrNoRows {
		return Identity{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Identity{}, fmt.Errorf("lookup identity %s: %w", id, err)
	}
	return i, nil
}

// NotFoundError reports an id that names no identity.
type NotFoundError struct{ ID string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("identity %s not found", e.ID) }

// Exists reports whether an identity id names a real identity
// (AC-identity-referenced: unknown ids are rejected, never created).
// Get returns one identity by id.
func Get(tx *sql.Tx, id string) (Identity, error) {
	var i Identity
	err := tx.QueryRow(`SELECT id, handle, kind, display_name FROM identities WHERE id = ?`, id).
		Scan(&i.ID, &i.Handle, &i.Kind, &i.DisplayName)
	if err != nil {
		return Identity{}, fmt.Errorf("get identity %s: %w", id, err)
	}
	return i, nil
}

func Exists(tx *sql.Tx, id string) (bool, error) {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM identities WHERE id = ?`, id).Scan(&one)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check identity %s: %w", id, err)
	}
	return true, nil
}
