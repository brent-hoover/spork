package issues

import (
	"database/sql"
	"fmt"
	"strings"
)

// Label mirrors the contract's Label schema. Labels live with issues
// (ENT-label relates many-to-many to ENT-issue) and join into Issue
// responses.
type Label struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Color *string `json:"color"`
}

// DuplicateLabelError reports a unique-violation on name, naming the
// colliding label for Error.conflicts (AC-unique-violation).
type DuplicateLabelError struct {
	Name       string
	ExistingID string
}

func (e *DuplicateLabelError) Error() string {
	return fmt.Sprintf("name %q already names label %s", e.Name, e.ExistingID)
}

// LabelNotFoundError reports an id that names no label.
type LabelNotFoundError struct{ ID string }

func (e *LabelNotFoundError) Error() string { return fmt.Sprintf("label %s not found", e.ID) }

// LabelAttachedError reports attaching a label an issue already
// carries; conflicts names the label.
type LabelAttachedError struct{ Label string }

func (e *LabelAttachedError) Error() string {
	return fmt.Sprintf("label %s is already attached", e.Label)
}

// LabelNotAttachedError reports detaching a label the issue does not
// carry.
type LabelNotAttachedError struct{ Label string }

func (e *LabelNotAttachedError) Error() string {
	return fmt.Sprintf("label %s is not attached", e.Label)
}

// MigrateLabels creates the label tables; called from Migrate.
func migrateLabels(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS labels (
			id    TEXT PRIMARY KEY,
			name  TEXT NOT NULL UNIQUE,
			color TEXT
		);
		CREATE TABLE IF NOT EXISTS issue_labels (
			issue TEXT NOT NULL REFERENCES issues(id),
			label TEXT NOT NULL REFERENCES labels(id),
			PRIMARY KEY (issue, label)
		);
	`)
	if err != nil {
		return fmt.Errorf("migrate labels: %w", err)
	}
	return nil
}

// CreateLabel inserts a label; the UNIQUE constraint is the authority
// on duplicates, translated by re-reading the holder (same race-safe
// shape as identity handles).
func CreateLabel(tx *sql.Tx, name string, color *string) (Label, error) {
	l := Label{ID: newUUIDv7(), Name: name, Color: color}
	_, err := tx.Exec(`INSERT INTO labels (id, name, color) VALUES (?, ?, ?)`, l.ID, l.Name, l.Color)
	if err == nil {
		return l, nil
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return Label{}, fmt.Errorf("insert label %q: %w", name, err)
	}
	var existing string
	if scanErr := tx.QueryRow(`SELECT id FROM labels WHERE name = ?`, name).Scan(&existing); scanErr != nil {
		return Label{}, fmt.Errorf("resolve duplicate label %q: %w", name, scanErr)
	}
	return Label{}, &DuplicateLabelError{Name: name, ExistingID: existing}
}

// ListLabels returns every label, name-ordered.
// LabelCursor streams labels from an opened query — names and colors
// are unconstrained, so the catalog is never accumulated (review 1922).
type LabelCursor struct{ rows *sql.Rows }

// OpenLabels runs the catalog query ordered by name.
func OpenLabels(tx *sql.Tx) (*LabelCursor, error) {
	rows, err := tx.Query(`SELECT id, name, color FROM labels ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	return &LabelCursor{rows: rows}, nil
}

func (c *LabelCursor) Each(fn func(Label) error) error {
	for c.rows.Next() {
		var l Label
		if err := c.rows.Scan(&l.ID, &l.Name, &l.Color); err != nil {
			return fmt.Errorf("scan label: %w", err)
		}
		if err := fn(l); err != nil {
			return err
		}
	}
	return c.rows.Err()
}

func (c *LabelCursor) Close() error { return c.rows.Close() }

// GetLabel returns one label.
// LabelIDsForProject returns the ids of every label attached to any
// issue in a project, ordered by label name — the SORT happens in SQL
// so a caller streaming the catalog never holds the names it orders by
// (review 1920).
func LabelIDsForProject(tx *sql.Tx, project string) ([]string, error) {
	rows, err := tx.Query(`SELECT DISTINCT l.id FROM labels l
		JOIN issue_labels il ON il.label = l.id
		JOIN issues i ON i.id = il.issue
		WHERE i.project = ? ORDER BY l.name`, project)
	if err != nil {
		return nil, fmt.Errorf("label ids for %s: %w", project, err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan label id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func GetLabel(tx *sql.Tx, id string) (Label, error) {
	var l Label
	err := tx.QueryRow(`SELECT id, name, color FROM labels WHERE id = ?`, id).Scan(&l.ID, &l.Name, &l.Color)
	if err == sql.ErrNoRows {
		return Label{}, &LabelNotFoundError{ID: id}
	}
	if err != nil {
		return Label{}, fmt.Errorf("get label %s: %w", id, err)
	}
	return l, nil
}

// AttachLabel ties a label to an issue (AC-issue-labels). Attaching a
// label the issue already carries conflicts, naming the label.
func AttachLabel(tx *sql.Tx, issueID, labelID string) error {
	if _, err := GetLabel(tx, labelID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO issue_labels (issue, label) VALUES (?, ?)`, issueID, labelID); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return &LabelAttachedError{Label: labelID}
		}
		return fmt.Errorf("attach label %s to %s: %w", labelID, issueID, err)
	}
	return nil
}

// DetachLabel unties a label; detaching one the issue does not carry
// is a 404-shaped error.
func DetachLabel(tx *sql.Tx, issueID, labelID string) error {
	res, err := tx.Exec(`DELETE FROM issue_labels WHERE issue = ? AND label = ?`, issueID, labelID)
	if err != nil {
		return fmt.Errorf("detach label %s from %s: %w", labelID, issueID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("detach label %s from %s: %w", labelID, issueID, err)
	}
	if n == 0 {
		return &LabelNotAttachedError{Label: labelID}
	}
	return nil
}

// LabelsOf returns an issue's current labels, name-ordered.
func LabelsOf(tx *sql.Tx, issueID string) ([]Label, error) {
	rows, err := tx.Query(`
		SELECT l.id, l.name, l.color FROM labels l
		JOIN issue_labels il ON il.label = l.id
		WHERE il.issue = ? ORDER BY l.name`, issueID)
	if err != nil {
		return nil, fmt.Errorf("labels of %s: %w", issueID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Label{}
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.ID, &l.Name, &l.Color); err != nil {
			return nil, fmt.Errorf("scan label: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
