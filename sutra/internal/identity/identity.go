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
	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// List returns identities, optionally filtered by kind.
func List(db *sql.DB, kind string) ([]Identity, error) {
	query, args := `SELECT id, handle, kind, display_name FROM identities ORDER BY handle`, []any{}
	if kind != "" {
		query, args = `SELECT id, handle, kind, display_name FROM identities WHERE kind = ? ORDER BY handle`, []any{kind}
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Identity
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.ID, &i.Handle, &i.Kind, &i.DisplayName); err != nil {
			return nil, fmt.Errorf("scan identity: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate identities: %w", err)
	}
	return out, nil
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
