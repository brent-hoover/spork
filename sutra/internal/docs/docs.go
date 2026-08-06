// Package docs — see MOD-docs in avspec.yaml. Planning artifacts live
// with the work they describe: documents file under a project,
// optionally tied to an issue, with append-only immutable versions
// (AC-doc-create, AC-doc-versioning) and name-managed templates that
// seed first versions (AC-template-crud, AC-template-instantiate).
package docs

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// Document mirrors the contract's Document schema.
type Document struct {
	ID             string  `json:"id"`
	Project        string  `json:"project"`
	Issue          *string `json:"issue"`
	Title          string  `json:"title"`
	CurrentVersion *string `json:"current_version"`
}

// Version mirrors DocVersion — immutable once written.
type Version struct {
	ID       string `json:"id"`
	Document string `json:"document"`
	Number   int64  `json:"number"`
	Content  string `json:"content"`
	Author   string `json:"author"`
	Created  string `json:"created"`
}

// Template mirrors DocTemplate.
type Template struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// NotFoundError reports an id that names no document.
type NotFoundError struct{ ID string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("document %s not found", e.ID) }

// VersionNotFoundError reports a missing version.
type VersionNotFoundError struct{ Ref string }

func (e *VersionNotFoundError) Error() string { return fmt.Sprintf("doc version %s not found", e.Ref) }

// TemplateNotFoundError reports an id that names no template.
type TemplateNotFoundError struct{ ID string }

func (e *TemplateNotFoundError) Error() string { return fmt.Sprintf("template %s not found", e.ID) }

// DuplicateTemplateNameError names the colliding template
// (AC-unique-violation).
type DuplicateTemplateNameError struct {
	Name       string
	ExistingID string
}

func (e *DuplicateTemplateNameError) Error() string {
	return fmt.Sprintf("template name %q already names %s", e.Name, e.ExistingID)
}

// Migrate creates the docs tables.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS documents (
			id              TEXT PRIMARY KEY,
			project         TEXT NOT NULL,
			issue           TEXT,
			title           TEXT NOT NULL,
			current_version TEXT
		);
		CREATE TABLE IF NOT EXISTS doc_versions (
			id       TEXT PRIMARY KEY,
			document TEXT NOT NULL REFERENCES documents(id),
			number   INTEGER NOT NULL,
			content  TEXT NOT NULL,
			author   TEXT NOT NULL,
			created  TEXT NOT NULL,
			UNIQUE (document, number)
		);
		CREATE TABLE IF NOT EXISTS doc_templates (
			id      TEXT PRIMARY KEY,
			name    TEXT NOT NULL UNIQUE,
			content TEXT NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("migrate docs: %w", err)
	}
	return nil
}

// Create files a document and its first version in one step.
func Create(tx *sql.Tx, project, title string, issue *string, content, author string) (Document, Version, error) {
	doc := Document{ID: newUUIDv7(), Project: project, Issue: issue, Title: title}
	if _, err := tx.Exec(`INSERT INTO documents (id, project, issue, title) VALUES (?, ?, ?, ?)`,
		doc.ID, project, issue, title); err != nil {
		return Document{}, Version{}, fmt.Errorf("insert document: %w", err)
	}
	version, err := SaveVersion(tx, doc.ID, content, author)
	if err != nil {
		return Document{}, Version{}, err
	}
	doc.CurrentVersion = &version.ID
	return doc, version, nil
}

// Get returns one document.
func Get(tx *sql.Tx, id string) (Document, error) {
	var d Document
	err := tx.QueryRow(`SELECT id, project, issue, title, current_version FROM documents WHERE id = ?`, id).
		Scan(&d.ID, &d.Project, &d.Issue, &d.Title, &d.CurrentVersion)
	if err == sql.ErrNoRows {
		return Document{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Document{}, fmt.Errorf("get document %s: %w", id, err)
	}
	return d, nil
}

// ListByProject returns a project's documents.
func ListByProject(tx *sql.Tx, project string) ([]Document, error) {
	return list(tx, `SELECT id, project, issue, title, current_version FROM documents WHERE project = ? ORDER BY title`, project)
}

// ListByIssue returns the documents tied to an issue.
func ListByIssue(tx *sql.Tx, issue string) ([]Document, error) {
	return list(tx, `SELECT id, project, issue, title, current_version FROM documents WHERE issue = ? ORDER BY title`, issue)
}

func list(tx *sql.Tx, query string, arg any) ([]Document, error) {
	rows, err := tx.Query(query, arg)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Document{}
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.Project, &d.Issue, &d.Title, &d.CurrentVersion); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SaveVersion appends the next immutable version and moves
// current_version (AC-doc-versioning).
func SaveVersion(tx *sql.Tx, documentID, content, author string) (Version, error) {
	if _, err := Get(tx, documentID); err != nil {
		return Version{}, err
	}
	var next int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(number), 0) + 1 FROM doc_versions WHERE document = ?`, documentID).Scan(&next); err != nil {
		return Version{}, fmt.Errorf("next version of %s: %w", documentID, err)
	}
	v := Version{
		ID: newUUIDv7(), Document: documentID, Number: next,
		Content: content, Author: author,
		Created: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if _, err := tx.Exec(`INSERT INTO doc_versions (id, document, number, content, author, created) VALUES (?, ?, ?, ?, ?, ?)`,
		v.ID, v.Document, v.Number, v.Content, v.Author, v.Created); err != nil {
		return Version{}, fmt.Errorf("insert version: %w", err)
	}
	if _, err := tx.Exec(`UPDATE documents SET current_version = ? WHERE id = ?`, v.ID, documentID); err != nil {
		return Version{}, fmt.Errorf("move current version: %w", err)
	}
	return v, nil
}

// VersionAt returns a document's version by number, or the latest for 0.
func VersionAt(tx *sql.Tx, documentID string, number int64) (Version, error) {
	query := `SELECT id, document, number, content, author, created FROM doc_versions WHERE document = ? ORDER BY number DESC LIMIT 1`
	args := []any{documentID}
	if number != 0 {
		query = `SELECT id, document, number, content, author, created FROM doc_versions WHERE document = ? AND number = ?`
		args = []any{documentID, number}
	}
	var v Version
	err := tx.QueryRow(query, args...).Scan(&v.ID, &v.Document, &v.Number, &v.Content, &v.Author, &v.Created)
	if err == sql.ErrNoRows {
		return Version{}, &VersionNotFoundError{Ref: fmt.Sprintf("%s#%d", documentID, number)}
	}
	if err != nil {
		return Version{}, fmt.Errorf("version %d of %s: %w", number, documentID, err)
	}
	return v, nil
}

// VersionByID returns one version addressed independently
// (AC-review-web renders doc deliverables through this).
func VersionByID(tx *sql.Tx, id string) (Version, error) {
	var v Version
	err := tx.QueryRow(`SELECT id, document, number, content, author, created FROM doc_versions WHERE id = ?`, id).
		Scan(&v.ID, &v.Document, &v.Number, &v.Content, &v.Author, &v.Created)
	if err == sql.ErrNoRows {
		return Version{}, &VersionNotFoundError{Ref: id}
	}
	if err != nil {
		return Version{}, fmt.Errorf("doc version %s: %w", id, err)
	}
	return v, nil
}

// ListVersions returns a document's history, oldest first
// (AC-doc-history).
func ListVersions(tx *sql.Tx, documentID string) ([]Version, error) {
	if _, err := Get(tx, documentID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT id, document, number, content, author, created FROM doc_versions WHERE document = ? ORDER BY number`, documentID)
	if err != nil {
		return nil, fmt.Errorf("versions of %s: %w", documentID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Version{}
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.Document, &v.Number, &v.Content, &v.Author, &v.Created); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetIssue ties or unties the document (AC-doc-link).
func SetIssue(tx *sql.Tx, documentID string, issue *string) (Document, error) {
	if _, err := Get(tx, documentID); err != nil {
		return Document{}, err
	}
	if _, err := tx.Exec(`UPDATE documents SET issue = ? WHERE id = ?`, issue, documentID); err != nil {
		return Document{}, fmt.Errorf("set issue of %s: %w", documentID, err)
	}
	return Get(tx, documentID)
}

// maxDiffCells bounds the LCS matrix (8 bytes per cell — 25M cells is
// 200 MB avoided; the fallback keeps memory linear).
const maxDiffCells = 4 << 20

// UnifiedDiff renders a line-based unified diff between two version
// contents (AC-doc-history). Common prefix and suffix are trimmed
// first — planning docs change in small regions — and the remaining
// middle uses a minimal LCS diff only while its matrix stays within
// maxDiffCells; beyond that the middle is emitted as one exact
// replacement hunk (still a correct old→new diff, just not minimal),
// keeping memory linear regardless of document shape.
func UnifiedDiff(from, to Version) string {
	a := strings.Split(from.Content, "\n")
	b := strings.Split(to.Content, "\n")

	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	midA := a[prefix : len(a)-suffix]
	midB := b[prefix : len(b)-suffix]

	var out strings.Builder
	fmt.Fprintf(&out, "--- v%d\n+++ v%d\n", from.Number, to.Number)
	// One whole-file hunk with full context — valid unified diff for
	// standard patch tooling.
	fmt.Fprintf(&out, "@@ -1,%d +1,%d @@\n", len(a), len(b))
	for _, line := range a[:prefix] {
		out.WriteString(" " + line + "\n")
	}
	if len(midA)*len(midB) <= maxDiffCells {
		writeLCSDiff(&out, midA, midB)
	} else {
		for _, line := range midA {
			out.WriteString("-" + line + "\n")
		}
		for _, line := range midB {
			out.WriteString("+" + line + "\n")
		}
	}
	for _, line := range a[len(a)-suffix:] {
		out.WriteString(" " + line + "\n")
	}
	return out.String()
}

func writeLCSDiff(out *strings.Builder, a, b []string) {
	lcs := make([][]int32, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out.WriteString(" " + a[i] + "\n")
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out.WriteString("-" + a[i] + "\n")
			i++
		default:
			out.WriteString("+" + b[j] + "\n")
			j++
		}
	}
	for ; i < len(a); i++ {
		out.WriteString("-" + a[i] + "\n")
	}
	for ; j < len(b); j++ {
		out.WriteString("+" + b[j] + "\n")
	}
}

// CreateTemplate inserts a template; the UNIQUE name constraint is the
// duplicate authority (AC-template-crud, AC-unique-violation).
func CreateTemplate(tx *sql.Tx, name, content string) (Template, error) {
	t := Template{ID: newUUIDv7(), Name: name, Content: content}
	_, err := tx.Exec(`INSERT INTO doc_templates (id, name, content) VALUES (?, ?, ?)`, t.ID, name, content)
	if err == nil {
		return t, nil
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return Template{}, fmt.Errorf("insert template %q: %w", name, err)
	}
	var existing string
	if scanErr := tx.QueryRow(`SELECT id FROM doc_templates WHERE name = ?`, name).Scan(&existing); scanErr != nil {
		return Template{}, fmt.Errorf("resolve duplicate template %q: %w", name, scanErr)
	}
	return Template{}, &DuplicateTemplateNameError{Name: name, ExistingID: existing}
}

// GetTemplate returns one template.
func GetTemplate(tx *sql.Tx, id string) (Template, error) {
	var t Template
	err := tx.QueryRow(`SELECT id, name, content FROM doc_templates WHERE id = ?`, id).Scan(&t.ID, &t.Name, &t.Content)
	if err == sql.ErrNoRows {
		return Template{}, &TemplateNotFoundError{ID: id}
	}
	if err != nil {
		return Template{}, fmt.Errorf("get template %s: %w", id, err)
	}
	return t, nil
}

// ListTemplates returns every template ordered by name.
func ListTemplates(tx *sql.Tx) ([]Template, error) {
	rows, err := tx.Query(`SELECT id, name, content FROM doc_templates ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Template{}
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.Name, &t.Content); err != nil {
			return nil, fmt.Errorf("scan template: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTemplate renames and/or replaces content; a rename into an
// existing name is the same unique-violation as creation.
func UpdateTemplate(tx *sql.Tx, id string, name, content *string) (Template, error) {
	if _, err := GetTemplate(tx, id); err != nil {
		return Template{}, err
	}
	sets, args := []string{}, []any{}
	if name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *name)
	}
	if content != nil {
		sets = append(sets, "content = ?")
		args = append(args, *content)
	}
	if len(sets) > 0 {
		args = append(args, id)
		if _, err := tx.Exec(`UPDATE doc_templates SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") && name != nil {
				var existing string
				if scanErr := tx.QueryRow(`SELECT id FROM doc_templates WHERE name = ?`, *name).Scan(&existing); scanErr != nil {
					return Template{}, fmt.Errorf("resolve rename collision %q: %w", *name, scanErr)
				}
				return Template{}, &DuplicateTemplateNameError{Name: *name, ExistingID: existing}
			}
			return Template{}, fmt.Errorf("update template %s: %w", id, err)
		}
	}
	return GetTemplate(tx, id)
}

// DeleteTemplate removes a template by id (AC-template-crud).
func DeleteTemplate(tx *sql.Tx, id string) error {
	res, err := tx.Exec(`DELETE FROM doc_templates WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete template %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return &TemplateNotFoundError{ID: id}
	}
	return nil
}

// newUUIDv7 returns an RFC 9562 UUIDv7 (CON-uuid-keys). Duplicated
// per-package; MOD-docs imports only its declared siblings.
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
