// Package identity — see MOD-identity in avspec.yaml for responsibility
// and boundaries. Identities are plain named kinds: a unique handle and
// human|agent, no credentials (AC-identity-create).
package identity

import (
	"crypto/rand"
	"database/sql"
	"fmt"
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

// Create inserts a new identity. A handle collision returns
// *DuplicateHandleError carrying the existing identity's id.
func Create(tx *sql.Tx, handle, kind string, displayName *string) (Identity, error) {
	var existing string
	err := tx.QueryRow(`SELECT id FROM identities WHERE handle = ?`, handle).Scan(&existing)
	switch {
	case err == nil:
		return Identity{}, &DuplicateHandleError{Handle: handle, ExistingID: existing}
	case err != sql.ErrNoRows:
		return Identity{}, fmt.Errorf("check handle %q: %w", handle, err)
	}
	id := newUUID()
	_, err = tx.Exec(`INSERT INTO identities (id, handle, kind, display_name) VALUES (?, ?, ?, ?)`,
		id, handle, kind, displayName)
	if err != nil {
		return Identity{}, fmt.Errorf("insert identity %q: %w", handle, err)
	}
	return Identity{ID: id, Handle: handle, Kind: kind, DisplayName: displayName}, nil
}

// newUUID returns a random RFC 4122 v4 UUID. Hand-rolled to keep the
// dependency surface at the spec's pinned set.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
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

// Exists reports whether an identity id names a real identity
// (AC-identity-referenced: unknown ids are rejected, never created).
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
