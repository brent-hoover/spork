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

// tsLayout is RFC 3339 with FIXED-WIDTH nanoseconds. time.RFC3339Nano
// trims trailing zeros, so its output does not sort lexically: with
// "…992647Z" against "…9926475Z", 'Z' > '5' and the earlier instant
// compares greater. Timestamps are stored and ordered as TEXT, so the
// format IS the ordering (review 1898).
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Document mirrors the contract's Document schema.
type Document struct {
	ID             string  `json:"id"`
	Project        string  `json:"project"`
	Issue          *string `json:"issue"`
	Title          string  `json:"title"`
	CurrentVersion *string `json:"current_version"`
}

// Version mirrors DocVersion — immutable once written.
// Version mirrors DocVersion. Content is declared LAST so metadata
// scans stop before it (review 1895).
type Version struct {
	ID       string `json:"id"`
	Document string `json:"document"`
	Number   int64  `json:"number"`
	Author   string `json:"author"`
	Created  string `json:"created"`
	Content  string `json:"content"`
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
		-- Document listings read one scope in title order; the index
		-- keeps that ordering off the temp-sort path, where it would
		-- hold every title (review 1924).
		CREATE INDEX IF NOT EXISTS documents_project_title ON documents(project, title);
		CREATE INDEX IF NOT EXISTS documents_issue_title ON documents(issue, title);
		-- An UNSCOPED search orders globally by title, which neither
		-- scoped index can serve — its leading column is the scope
		-- (review 1928).
		CREATE INDEX IF NOT EXISTS documents_title ON documents(title);
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

// VersionsEach streams a document's versions oldest-first — export
// assembles gigabyte-capable content without accumulating it.
func VersionsEach(tx *sql.Tx, documentID string, fn func(Version) error) error {
	rows, err := tx.Query(`SELECT id, document, number, content, author, created FROM doc_versions WHERE document = ? ORDER BY number`, documentID)
	if err != nil {
		return fmt.Errorf("versions of %s: %w", documentID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.Document, &v.Number, &v.Content, &v.Author, &v.Created); err != nil {
			return fmt.Errorf("scan version: %w", err)
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// VersionMetaByID returns one version WITHOUT its content — deliverable
// validation needs existence and lineage, never the bytes.
func VersionMetaByID(tx *sql.Tx, id string) (Version, error) {
	var v Version
	err := tx.QueryRow(`SELECT id, document, number, author, created FROM doc_versions WHERE id = ?`, id).
		Scan(&v.ID, &v.Document, &v.Number, &v.Author, &v.Created)
	if err == sql.ErrNoRows {
		return Version{}, &VersionNotFoundError{Ref: id}
	}
	if err != nil {
		return Version{}, fmt.Errorf("version meta %s: %w", id, err)
	}
	return v, nil
}

// Meta is the fixed-size facts about a document: identity, ownership,
// current version pointer. The title is unbounded, so callers that only
// need scope or the current-version pointer must not read it — that is
// the whole point of the bounded document endpoint (review 1920).
type Meta struct {
	ID             string
	Project        string
	Issue          *string
	CurrentVersion *string
}

// MetaByID reads that projection; the title column is never selected.
func MetaByID(tx *sql.Tx, id string) (Meta, error) {
	var m Meta
	err := tx.QueryRow(`SELECT id, project, issue, current_version FROM documents WHERE id = ?`, id).
		Scan(&m.ID, &m.Project, &m.Issue, &m.CurrentVersion)
	if err == sql.ErrNoRows {
		return Meta{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Meta{}, fmt.Errorf("document meta %s: %w", id, err)
	}
	return m, nil
}

// SearchIDs is Search's discovery projection: matching ids in the same
// order, without the titles. Callers that stream documents to the wire
// must use it — titles are unbounded, so an accumulated result set is
// not memory-bounded no matter how the rows are written (review 1918).
func SearchIDs(tx *sql.Tx, project *string, q string) ([]string, error) {
	term := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	query := `SELECT id FROM documents
		WHERE (title LIKE ? ESCAPE '\'
			OR EXISTS (SELECT 1 FROM doc_versions v WHERE v.document = documents.id AND v.content LIKE ? ESCAPE '\'))`
	args := []any{term, term}
	if project != nil {
		query += ` AND project = ?`
		args = append(args, *project)
	}
	return ids(tx, query+` ORDER BY title`, args...)
}

// IDsByProject is ListByProject's discovery projection.
func IDsByProject(tx *sql.Tx, project string) ([]string, error) {
	return ids(tx, `SELECT id FROM documents WHERE project = ? ORDER BY title`, project)
}

func ids(tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list document ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan document id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DocumentCursor streams documents from an opened query — titles are
// unconstrained, so a listing never accumulates them (review 1922).
type DocumentCursor struct{ rows *sql.Rows }

// OpenByProject and OpenByIssue run the two document listings, each
// ordered by title.
func OpenByProject(tx *sql.Tx, project string) (*DocumentCursor, error) {
	return openDocuments(tx, `SELECT id, project, issue, title, current_version FROM documents WHERE project = ? ORDER BY title`, project)
}

func OpenByIssue(tx *sql.Tx, issue string) (*DocumentCursor, error) {
	return openDocuments(tx, `SELECT id, project, issue, title, current_version FROM documents WHERE issue = ? ORDER BY title`, issue)
}

func openDocuments(tx *sql.Tx, query string, args ...any) (*DocumentCursor, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	return &DocumentCursor{rows: rows}, nil
}

func (c *DocumentCursor) Each(fn func(Document) error) error {
	for c.rows.Next() {
		var d Document
		if err := c.rows.Scan(&d.ID, &d.Project, &d.Issue, &d.Title, &d.CurrentVersion); err != nil {
			return fmt.Errorf("scan document: %w", err)
		}
		if err := fn(d); err != nil {
			return err
		}
	}
	return c.rows.Err()
}

func (c *DocumentCursor) Close() error { return c.rows.Close() }

// SaveVersion appends the next immutable version and moves
// current_version (AC-doc-versioning).
func SaveVersion(tx *sql.Tx, documentID, content, author string) (Version, error) {
	// Existence, not the document — Get reads the unbounded title,
	// and this runs inside the caller's write transaction (1936).
	if _, err := MetaByID(tx, documentID); err != nil {
		return Version{}, err
	}
	var next int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(number), 0) + 1 FROM doc_versions WHERE document = ?`, documentID).Scan(&next); err != nil {
		return Version{}, fmt.Errorf("next version of %s: %w", documentID, err)
	}
	v := Version{
		ID: newUUIDv7(), Document: documentID, Number: next,
		Content: content, Author: author,
		Created: time.Now().UTC().Format(tsLayout),
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

// CurrentVersionMeta is VersionAt(current) without the content column,
// so a caller that needs only the version number never reads an
// unbounded value. The live-refresh poll runs every five seconds
// (review 1912).
func CurrentVersionMeta(tx *sql.Tx, documentID string) (Version, error) {
	var v Version
	err := tx.QueryRow(`SELECT id, document, number, author, created FROM doc_versions
		WHERE document = ? ORDER BY number DESC LIMIT 1`, documentID).
		Scan(&v.ID, &v.Document, &v.Number, &v.Author, &v.Created)
	if err == sql.ErrNoRows {
		return Version{}, &VersionNotFoundError{Ref: documentID}
	}
	if err != nil {
		return Version{}, fmt.Errorf("current version meta of %s: %w", documentID, err)
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

// diffSide is one version's content split into pure lines plus an
// out-of-band termination flag — never embedded in line text, so
// documents containing NUL (or any byte) diff faithfully.
type diffSide struct {
	lines      []string
	terminated bool
}

func splitSide(content string) diffSide {
	if content == "" {
		return diffSide{terminated: true}
	}
	lines := strings.Split(content, "\n")
	if lines[len(lines)-1] == "" {
		return diffSide{lines: lines[:len(lines)-1], terminated: true}
	}
	return diffSide{lines: lines, terminated: false}
}

// lineEqual compares by text AND final-line termination compatibility:
// an unterminated final line never pairs with a terminated twin.
func lineEqual(a diffSide, i int, b diffSide, j int) bool {
	if a.lines[i] != b.lines[j] {
		return false
	}
	aFinalUnterminated := i == len(a.lines)-1 && !a.terminated
	bFinalUnterminated := j == len(b.lines)-1 && !b.terminated
	return aFinalUnterminated == bFinalUnterminated
}

// emit writes one diff line; the no-newline marker follows a side's
// unterminated final line.
func emit(out *strings.Builder, mark byte, s diffSide, idx int) {
	out.WriteByte(mark)
	out.WriteString(s.lines[idx])
	out.WriteByte('\n')
	if idx == len(s.lines)-1 && !s.terminated {
		out.WriteString("\\ No newline at end of file\n")
	}
}

// maxDiffCells bounds the LCS matrix (4 bytes per cell); beyond it the
// fallback keeps memory linear.
const maxDiffCells = 4 << 20

// MaxDiffInput bounds the combined content size the diff endpoint will
// process — diffing materializes both versions plus output, so the
// bound keeps a single request's memory at a small multiple of this.
var MaxDiffInput = int64(64 << 20)

// DiffTooLargeError reports versions beyond the documented diff bound.
type DiffTooLargeError struct{ Combined int64 }

func (e *DiffTooLargeError) Error() string {
	return fmt.Sprintf("combined version size %d exceeds the %d-byte diff bound", e.Combined, MaxDiffInput)
}

// VersionSizesOK validates the two versions' combined stored size via
// SQL BEFORE any content loads — the bound rejects without
// materializing what it guards against. Sizes are measured in BYTES
// (length of the BLOB cast; plain length(TEXT) counts characters), and
// identical from/to numbers count twice because both sides load.
func VersionSizesOK(tx *sql.Tx, documentID string, from, to int64) error {
	var combined int64
	for _, number := range []int64{from, to} {
		var size sql.NullInt64
		err := tx.QueryRow(`
			SELECT length(CAST(content AS BLOB)) FROM doc_versions
			WHERE document = ? AND number = ?`, documentID, number).Scan(&size)
		if err == sql.ErrNoRows {
			continue // the load path reports the 404
		}
		if err != nil {
			return fmt.Errorf("size version %d of %s: %w", number, documentID, err)
		}
		combined += size.Int64
	}
	if combined > MaxDiffInput {
		return &DiffTooLargeError{Combined: combined}
	}
	return nil
}

// maxDiffLines bounds the combined LINE count a diff will process:
// splitting materializes one string header (16 bytes) per line, so
// newline-dense content within the byte bound could otherwise multiply
// memory ~16x. 8M lines caps the header overhead at ~128 MiB.
var maxDiffLines = int64(8 << 20)

// DiffTooDenseError reports versions whose combined line count exceeds
// the documented diff bound.
type DiffTooDenseError struct{ Lines int64 }

func (e *DiffTooDenseError) Error() string {
	return fmt.Sprintf("combined line count %d exceeds the %d-line diff bound", e.Lines, maxDiffLines)
}

// CheckDiffable validates the loaded contents' combined line density
// before UnifiedDiff splits them — counting allocates nothing.
func CheckDiffable(from, to Version) error {
	// Empty content is zero lines, and newline-TERMINATED content has
	// exactly its newline count — the trailing newline ends the last
	// line rather than starting another. Adding one unconditionally
	// rejected valid input at the documented limit (review 1934).
	countLines := func(content string) int64 {
		if content == "" {
			return 0
		}
		n := int64(strings.Count(content, "\n"))
		if !strings.HasSuffix(content, "\n") {
			n++
		}
		return n
	}
	lines := countLines(from.Content) + countLines(to.Content)
	if lines > maxDiffLines {
		return &DiffTooDenseError{Lines: lines}
	}
	return nil
}

// UnifiedDiff renders a line-based unified diff between two version
// contents (AC-doc-history) that standard patch tooling can apply:
// real line counts, no-newline markers via out-of-band termination
// state, and a whole-file hunk with full context. Common prefix and
// suffix are trimmed first; the remaining middle uses a minimal LCS
// diff while its matrix stays within maxDiffCells and an exact
// replacement hunk beyond that — correct output, linear memory.
func UnifiedDiff(from, to Version) string {
	a := splitSide(from.Content)
	b := splitSide(to.Content)

	prefix := 0
	for prefix < len(a.lines) && prefix < len(b.lines) && lineEqual(a, prefix, b, prefix) {
		prefix++
	}
	suffix := 0
	for suffix < len(a.lines)-prefix && suffix < len(b.lines)-prefix &&
		lineEqual(a, len(a.lines)-1-suffix, b, len(b.lines)-1-suffix) {
		suffix++
	}
	midA := len(a.lines) - prefix - suffix
	midB := len(b.lines) - prefix - suffix

	var out strings.Builder
	fmt.Fprintf(&out, "--- v%d\n+++ v%d\n", from.Number, to.Number)
	// Zero-length ranges start at 0 per the unified-diff format.
	fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", hunkStart(len(a.lines)), len(a.lines), hunkStart(len(b.lines)), len(b.lines))
	for i := 0; i < prefix; i++ {
		emit(&out, ' ', a, i)
	}
	// An empty side needs no LCS: the middle is a pure deletion or
	// pure addition. The guard checks the ACTUAL matrix dimensions
	// ((midA+1)x(midB+1)) with division, never a product, so neither
	// overflow nor a zero side can slip an unbounded allocation past
	// it; beyond the bound the exact-replacement fallback emits the
	// same pure runs.
	if midA == 0 || midB == 0 || (midA+1) > maxDiffCells/(midB+1) {
		for i := prefix; i < prefix+midA; i++ {
			emit(&out, '-', a, i)
		}
		for j := prefix; j < prefix+midB; j++ {
			emit(&out, '+', b, j)
		}
	} else {
		writeLCSDiff(&out, a, b, prefix, midA, midB)
	}
	for i := len(a.lines) - suffix; i < len(a.lines); i++ {
		emit(&out, ' ', a, i)
	}
	return out.String()
}

func hunkStart(count int) int {
	if count == 0 {
		return 0
	}
	return 1
}

func writeLCSDiff(out *strings.Builder, a, b diffSide, offset, lenA, lenB int) {
	lcs := make([][]int32, lenA+1)
	for i := range lcs {
		lcs[i] = make([]int32, lenB+1)
	}
	for i := lenA - 1; i >= 0; i-- {
		for j := lenB - 1; j >= 0; j-- {
			if lineEqual(a, offset+i, b, offset+j) {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < lenA && j < lenB {
		switch {
		case lineEqual(a, offset+i, b, offset+j):
			emit(out, ' ', a, offset+i)
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			emit(out, '-', a, offset+i)
			i++
		default:
			emit(out, '+', b, offset+j)
			j++
		}
	}
	for ; i < lenA; i++ {
		emit(out, '-', a, offset+i)
	}
	for ; j < lenB; j++ {
		emit(out, '+', b, offset+j)
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

// TemplateCursor streams the catalog AFTER its query has succeeded.
// Opening is separate from iterating so a caller can commit an HTTP
// status only once the read is known to have started: writing 200 first
// would turn a query failure into a truncated success (review 1918).
type TemplateCursor struct{ rows *sql.Rows }

// OpenTemplates runs the catalog query ordered by name. Template
// content is unbounded like every other content field, so rows are
// streamed, never accumulated (review 1916).
func OpenTemplates(tx *sql.Tx) (*TemplateCursor, error) {
	rows, err := tx.Query(`SELECT id, name, content FROM doc_templates ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	return &TemplateCursor{rows: rows}, nil
}

// Each delivers every row, one at a time.
func (c *TemplateCursor) Each(fn func(Template) error) error {
	for c.rows.Next() {
		var t Template
		if err := c.rows.Scan(&t.ID, &t.Name, &t.Content); err != nil {
			return fmt.Errorf("scan template: %w", err)
		}
		if err := fn(t); err != nil {
			return err
		}
	}
	return c.rows.Err()
}

func (c *TemplateCursor) Close() error { return c.rows.Close() }

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
