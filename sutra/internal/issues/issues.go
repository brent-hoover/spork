// Package issues — see MOD-issues in avspec.yaml. Owns issues, their
// per-project display numbers, the parent/blocks relation graph, the
// subtree_revision fence, and the reopen cascade. Every function runs
// inside the caller's transaction; the api module owns transactions,
// event emission, and the archived-project guard.
package issues

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

// Issue mirrors the contract's Issue schema. Declaration order IS wire
// order, and the UI's ownership guards depend on it: Project comes
// FIRST so a scope check stops before title and labels — which the
// contract leaves unbounded (review 1900) — and Body comes LAST so a
// metadata scan never materializes it at all (review 1895). JSON
// property order is insignificant to consumers.
type Issue struct {
	ID              string  `json:"id"`
	Project         string  `json:"project"`
	Number          int64   `json:"number"`
	Status          string  `json:"status"`
	Assignee        *string `json:"assignee,omitempty"`
	Created         string  `json:"created"`
	Updated         string  `json:"updated"`
	SubtreeRevision int64   `json:"subtree_revision"`
	Title           string  `json:"title"`
	Labels          []Label `json:"labels,omitempty"`
	Body            *string `json:"body,omitempty"`
}

// Relation mirrors the contract's IssueRelation schema.
type Relation struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Statuses the store accepts; complete is reachable only through the
// review-gated transition.
const (
	StatusOpen       = "open"
	StatusInProgress = "in-progress"
	StatusBlocked    = "blocked"
	StatusDeferred   = "deferred"
	StatusComplete   = "complete"
)

// Active reports whether a status holds work open for the close gate
// and the reopen cascade: open, in-progress, or blocked — deferred and
// complete are not active.
func Active(status string) bool {
	return status == StatusOpen || status == StatusInProgress || status == StatusBlocked
}

// NotFoundError reports an id that names no issue.
type NotFoundError struct{ ID string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("issue %s not found", e.ID) }

// RelationNotFoundError reports an id that names no relation.
type RelationNotFoundError struct{ ID string }

func (e *RelationNotFoundError) Error() string { return fmt.Sprintf("relation %s not found", e.ID) }

// RelationExistsError reports an already-present relation
// (AC-relation-conflict-codes: code relation-exists).
type RelationExistsError struct{ Existing string }

func (e *RelationExistsError) Error() string {
	return fmt.Sprintf("relation already exists as %s", e.Existing)
}

// CycleError reports an ancestry or blocking cycle; Kind selects the
// normative conflict code (ancestry-cycle | blocking-cycle).
type CycleError struct{ Kind string }

func (e *CycleError) Error() string { return fmt.Sprintf("%s rejected: cycle", e.Kind) }

// HasParentError reports a second parent_of onto a child that has one.
type HasParentError struct{ Child, Parent string }

func (e *HasParentError) Error() string {
	return fmt.Sprintf("issue %s already has parent %s", e.Child, e.Parent)
}

// Migrate creates the issues tables.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS issues (
			id               TEXT PRIMARY KEY,
			project          TEXT NOT NULL,
			number           INTEGER NOT NULL,
			title            TEXT NOT NULL,
			body             TEXT,
			status           TEXT NOT NULL DEFAULT 'open'
				CHECK (status IN ('open', 'in-progress', 'blocked', 'deferred', 'complete')),
			assignee         TEXT,
			assigned_at      TEXT,
			subtree_revision INTEGER NOT NULL DEFAULT 0,
			created          TEXT NOT NULL,
			updated          TEXT NOT NULL,
			UNIQUE (project, number)
		);
		CREATE TABLE IF NOT EXISTS issue_numbers (
			project TEXT PRIMARY KEY,
			next    INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS issue_relations (
			id         TEXT PRIMARY KEY,
			kind       TEXT NOT NULL CHECK (kind IN ('parent_of', 'blocks')),
			from_issue TEXT NOT NULL REFERENCES issues(id),
			to_issue   TEXT NOT NULL REFERENCES issues(id),
			UNIQUE (kind, from_issue, to_issue)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS issue_single_parent
			ON issue_relations (to_issue) WHERE kind = 'parent_of';
	`)
	if err != nil {
		return fmt.Errorf("migrate issues: %w", err)
	}
	return migrateLabels(db)
}

// Create mints a UUIDv7 id and the next per-project display number
// (AC-issue-create); status starts open.
func Create(tx *sql.Tx, project, title string, body, assignee *string) (Issue, error) {
	var number int64
	err := tx.QueryRow(`
		INSERT INTO issue_numbers (project, next) VALUES (?, 1)
		ON CONFLICT (project) DO UPDATE SET next = next + 1
		RETURNING next`, project).Scan(&number)
	if err != nil {
		return Issue{}, fmt.Errorf("mint number for %s: %w", project, err)
	}
	now := time.Now().UTC().Format(tsLayout)
	issue := Issue{
		ID: newUUIDv7(), Number: number, Title: title, Body: body,
		Status: StatusOpen, Project: project, Assignee: assignee,
		Created: now, Updated: now,
	}
	// A creation-time assignee joins the work stack NOW: assigned_at
	// stamps here too, or FIFO ordering would put the null first.
	var assignedAt *string
	if assignee != nil {
		assignedAt = &now
	}
	_, err = tx.Exec(`
		INSERT INTO issues (id, project, number, title, body, status, assignee, assigned_at, subtree_revision, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		issue.ID, project, number, title, body, StatusOpen, assignee, assignedAt, now, now)
	if err != nil {
		return Issue{}, fmt.Errorf("insert issue: %w", err)
	}
	return issue, nil
}

const issueColumns = `id, number, title, body, status, project, assignee, created, updated, subtree_revision`

type rowScanner interface{ Scan(...any) error }

func scanIssue(r rowScanner) (Issue, error) {
	var i Issue
	if err := r.Scan(&i.ID, &i.Number, &i.Title, &i.Body, &i.Status, &i.Project, &i.Assignee, &i.Created, &i.Updated, &i.SubtreeRevision); err != nil {
		return Issue{}, err
	}
	return i, nil
}

// Get returns one issue inside a transaction.
func Get(tx *sql.Tx, id string) (Issue, error) {
	i, err := scanIssue(tx.QueryRow(`SELECT `+issueColumns+` FROM issues WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Issue{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Issue{}, fmt.Errorf("get issue %s: %w", id, err)
	}
	labels, err := LabelsOf(tx, id)
	if err != nil {
		return Issue{}, err
	}
	// Assigned unconditionally: LabelsOf returns nil for an unlabelled
	// issue and the field is omitempty, so guarding on the length was a
	// branch whose two arms produce the same bytes on the wire.
	i.Labels = labels
	return i, nil
}

// Filters narrows List; every filter composes with the others by AND
// (AC-search-filters). Q is a free-text term over title, body, and
// comment bodies (AC-search-text); Labels are label NAMES the issue
// must all carry.
type Filters struct {
	Number   *int64
	Statuses []string
	Assignee string
	Labels   []string
	Q        string
}

// escapeLike neutralizes LIKE wildcards in a user term.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Ref is a light issue reference for discovery listings — no body.
type Ref struct {
	ID      string
	Project string
	Number  int64
}

// RefByID reads one issue's reference. Callers deciding whether an
// issue belongs in a result set need its project and number, never its
// body or labels — those load only for the records that survive
// (review 1920).
func RefByID(tx *sql.Tx, id string) (Ref, error) {
	var r Ref
	err := tx.QueryRow(`SELECT id, project, number FROM issues WHERE id = ?`, id).
		Scan(&r.ID, &r.Project, &r.Number)
	if err == sql.ErrNoRows {
		return Ref{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Ref{}, fmt.Errorf("issue ref %s: %w", id, err)
	}
	return r, nil
}

// filterClause builds the WHERE clause every project-scoped issue query
// shares. It is stated ONCE on purpose: a filter written out twice is
// two places a rule can drift apart, and neither copy can be falsified
// by a test that only reaches the other.
func filterClause(project string, f Filters) (string, []any) {
	clause := ` WHERE project = ?`
	args := []any{project}
	if f.Number != nil {
		clause += ` AND number = ?`
		args = append(args, *f.Number)
	}
	if len(f.Statuses) > 0 {
		clause += ` AND status IN (?` + strings.Repeat(",?", len(f.Statuses)-1) + `)`
		for _, s := range f.Statuses {
			args = append(args, s)
		}
	}
	if f.Assignee != "" {
		clause += ` AND assignee = ?`
		args = append(args, f.Assignee)
	}
	for _, name := range f.Labels {
		clause += ` AND EXISTS (SELECT 1 FROM issue_labels il JOIN labels l ON l.id = il.label
			WHERE il.issue = issues.id AND l.name = ?)`
		args = append(args, name)
	}
	if f.Q != "" {
		term := "%" + escapeLike(f.Q) + "%"
		// The comments table joins by column, not by package import —
		// module boundaries constrain code, the schema is shared.
		clause += ` AND (title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\'
			OR EXISTS (SELECT 1 FROM comments c WHERE c.issue = issues.id AND c.body LIKE ? ESCAPE '\'))`
		args = append(args, term, term, term)
	}
	return clause, args
}

// SearchIDs runs List's filters but returns only references — search
// discovery never loads unbounded bodies it will reduce to ids anyway.
func SearchIDs(tx *sql.Tx, project string, f Filters) ([]Ref, error) {
	clause, args := filterClause(project, f)
	rows, err := tx.Query(`SELECT id, project, number FROM issues`+clause+` ORDER BY number`, args...)
	if err != nil {
		return nil, fmt.Errorf("search issue ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Ref{}
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.ID, &r.Project, &r.Number); err != nil {
			return nil, fmt.Errorf("scan issue ref: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListEach streams a project's filtered issues one row at a time, in
// filter order (Q ranks title, then body, then comments; number
// otherwise). Bodies are unbounded strings — wire-serving listings
// must never accumulate them, so the filter pass collects only ids
// and each issue loads as it is handed over.
func ListEach(tx *sql.Tx, project string, f Filters, fn func(Issue) error) error {
	query, args := filterClause(project, f)
	query = `SELECT id FROM issues` + query
	// A text search ranks its hits — title before body before comment —
	// where an unfiltered listing has only number to go on.
	if f.Q != "" {
		term := "%" + escapeLike(f.Q) + "%"
		query += ` ORDER BY CASE WHEN title LIKE ? ESCAPE '\' THEN 0 WHEN body LIKE ? ESCAPE '\' THEN 1 ELSE 2 END, number`
		args = append(args, term, term)
	} else {
		query += ` ORDER BY number`
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return fmt.Errorf("list issues: %w", err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan issue id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate issues: %w", err)
	}
	_ = rows.Close()
	for _, id := range ids {
		issue, err := Get(tx, id)
		if err != nil {
			return err
		}
		if err := fn(issue); err != nil {
			return err
		}
	}
	return nil
}

// Update rewrites title/body and stamps updated (AC-issue-update).
func Update(tx *sql.Tx, id string, title, body *string) (Issue, error) {
	if _, err := Get(tx, id); err != nil {
		return Issue{}, err
	}
	sets, args := []string{"updated = ?"}, []any{time.Now().UTC().Format(tsLayout)}
	if title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *title)
	}
	if body != nil {
		sets = append(sets, "body = ?")
		args = append(args, *body)
	}
	args = append(args, id)
	if _, err := tx.Exec(`UPDATE issues SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return Issue{}, fmt.Errorf("update issue %s: %w", id, err)
	}
	return Get(tx, id)
}

// Ancestors returns the parent chain of id, nearest first.
func Ancestors(tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.Query(`
		WITH RECURSIVE chain(id) AS (
			SELECT from_issue FROM issue_relations WHERE kind = 'parent_of' AND to_issue = ?
			UNION
			SELECT r.from_issue FROM issue_relations r JOIN chain c ON r.to_issue = c.id AND r.kind = 'parent_of'
		) SELECT id FROM chain`, id)
	if err != nil {
		return nil, fmt.Errorf("ancestors of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("scan ancestor: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ActiveDescendants returns the open, in-progress, or blocked issues in
// id's subtree — id excluded — at any depth, including beneath deferred
// children (AC-parent-close-gate). The ids name exactly the work a
// rejected close must surface.
func ActiveDescendants(tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.Query(`
		WITH RECURSIVE sub(id) AS (
			SELECT to_issue FROM issue_relations WHERE kind = 'parent_of' AND from_issue = ?
			UNION
			SELECT r.to_issue FROM issue_relations r JOIN sub s ON r.from_issue = s.id AND r.kind = 'parent_of'
		) SELECT id FROM issues WHERE id IN (SELECT id FROM sub)
		  AND status IN ('open', 'in-progress', 'blocked') ORDER BY number`, id)
	if err != nil {
		return nil, fmt.Errorf("active-descendant check for %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("scan active descendant: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ActiveInSubtree reports whether any active descendant exists —
// LIMIT 1, never materializing the subtree; the close gate uses
// ActiveDescendants when it must NAME the blockers.
func ActiveInSubtree(tx *sql.Tx, id string) (bool, error) {
	var one int
	err := tx.QueryRow(`
		WITH RECURSIVE sub(id) AS (
			SELECT to_issue FROM issue_relations WHERE kind = 'parent_of' AND from_issue = ?
			UNION
			SELECT r.to_issue FROM issue_relations r JOIN sub s ON r.from_issue = s.id AND r.kind = 'parent_of'
		) SELECT 1 FROM issues WHERE id IN (SELECT id FROM sub)
		  AND status IN ('open', 'in-progress', 'blocked') LIMIT 1`, id).Scan(&one)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("active-descendant check for %s: %w", id, err)
	}
	return true, nil
}

// AddRelation creates a parent_of or blocks relation after cycle,
// duplicate, and single-parent checks (AC-parent-create,
// AC-block-no-cycles, AC-relation-conflict-codes).
func AddRelation(tx *sql.Tx, kind, from, to string) (Relation, error) {
	for _, id := range []string{from, to} {
		if _, err := Get(tx, id); err != nil {
			return Relation{}, err
		}
	}
	if from == to {
		return Relation{}, &CycleError{Kind: kind}
	}
	var existing string
	err := tx.QueryRow(`SELECT id FROM issue_relations WHERE kind = ? AND from_issue = ? AND to_issue = ?`,
		kind, from, to).Scan(&existing)
	if err == nil {
		return Relation{}, &RelationExistsError{Existing: existing}
	}
	if err != sql.ErrNoRows {
		return Relation{}, fmt.Errorf("check relation: %w", err)
	}
	// A cycle exists iff `from` is reachable FROM `to` along kind edges.
	var one int
	err = tx.QueryRow(`
		WITH RECURSIVE reach(id) AS (
			SELECT to_issue FROM issue_relations WHERE kind = ? AND from_issue = ?
			UNION
			SELECT r.to_issue FROM issue_relations r JOIN reach c ON r.from_issue = c.id AND r.kind = ?
		) SELECT 1 FROM reach WHERE id = ? LIMIT 1`, kind, to, kind, from).Scan(&one)
	switch {
	case err == nil:
		return Relation{}, &CycleError{Kind: kind}
	case err != sql.ErrNoRows:
		return Relation{}, fmt.Errorf("cycle check: %w", err)
	}
	if kind == "parent_of" {
		var parent string
		err := tx.QueryRow(`SELECT from_issue FROM issue_relations WHERE kind = 'parent_of' AND to_issue = ?`, to).Scan(&parent)
		if err == nil {
			return Relation{}, &HasParentError{Child: to, Parent: parent}
		}
		if err != sql.ErrNoRows {
			return Relation{}, fmt.Errorf("parent check: %w", err)
		}
	}
	rel := Relation{ID: newUUIDv7(), Kind: kind, From: from, To: to}
	if _, err := tx.Exec(`INSERT INTO issue_relations (id, kind, from_issue, to_issue) VALUES (?, ?, ?, ?)`,
		rel.ID, rel.Kind, rel.From, rel.To); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Relation{}, &RelationExistsError{Existing: "concurrent"}
		}
		return Relation{}, fmt.Errorf("insert relation: %w", err)
	}
	return rel, nil
}

// Relations returns every relation where id is either side.
func Relations(tx *sql.Tx, id string) ([]Relation, error) {
	rows, err := tx.Query(`
		SELECT id, kind, from_issue, to_issue FROM issue_relations
		WHERE from_issue = ? OR to_issue = ? ORDER BY id`, id, id)
	if err != nil {
		return nil, fmt.Errorf("relations of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Relation{}
	for rows.Next() {
		var r Relation
		if err := rows.Scan(&r.ID, &r.Kind, &r.From, &r.To); err != nil {
			return nil, fmt.Errorf("scan relation: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RemoveRelation deletes a relation and returns its snapshot for the
// issue.relation-removed event payload.
func RemoveRelation(tx *sql.Tx, id string) (Relation, error) {
	var r Relation
	err := tx.QueryRow(`SELECT id, kind, from_issue, to_issue FROM issue_relations WHERE id = ?`, id).
		Scan(&r.ID, &r.Kind, &r.From, &r.To)
	if err == sql.ErrNoRows {
		return Relation{}, &RelationNotFoundError{ID: id}
	}
	if err != nil {
		return Relation{}, fmt.Errorf("get relation %s: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM issue_relations WHERE id = ?`, id); err != nil {
		return Relation{}, fmt.Errorf("delete relation %s: %w", id, err)
	}
	return r, nil
}

// BumpSubtree increments subtree_revision by exactly one on every
// distinct id given — the once-per-transaction guarantee of
// AC-subtree-revision is the caller's: it collects each affected issue
// and its ancestors, dedupes, and calls this once.
func BumpSubtree(tx *sql.Tx, ids []string) error {
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.Exec(`UPDATE issues SET subtree_revision = subtree_revision + 1 WHERE id = ?`, id); err != nil {
			return fmt.Errorf("bump subtree_revision on %s: %w", id, err)
		}
	}
	return nil
}

// SetStatus writes a status and stamps updated. Gate checks live with
// the API transition, which owns ordering and events.
func SetStatus(tx *sql.Tx, id, status string) error {
	res, err := tx.Exec(`UPDATE issues SET status = ?, updated = ? WHERE id = ?`,
		status, time.Now().UTC().Format(tsLayout), id)
	if err != nil {
		return fmt.Errorf("set status of %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return &NotFoundError{ID: id}
	}
	return nil
}

// Assign sets or clears the assignee (AC-issue-assign); assignment
// order feeds the work stack, so assigned_at stamps on set and clears
// with the assignee.
func Assign(tx *sql.Tx, id string, assignee *string) (Issue, error) {
	if _, err := Get(tx, id); err != nil {
		return Issue{}, err
	}
	now := time.Now().UTC().Format(tsLayout)
	var assignedAt *string
	if assignee != nil {
		assignedAt = &now
	}
	if _, err := tx.Exec(`UPDATE issues SET assignee = ?, assigned_at = ?, updated = ? WHERE id = ?`,
		assignee, assignedAt, now, id); err != nil {
		return Issue{}, fmt.Errorf("assign issue %s: %w", id, err)
	}
	return Get(tx, id)
}

// PopCandidate resolves the issue a pop should claim for an identity:
// the oldest-assigned workable issue, walked to the deepest open
// self-assigned blocker (AC-pop-fifo, AC-pop-blocker-first). An issue
// blocked by live work assigned elsewhere — or otherwise unworkable —
// is skipped (AC-pop-skips-blocked); archived projects' issues are
// excluded by the caller's join. Returns nil when nothing is workable.
func PopCandidate(tx *sql.Tx, identity string) (*Issue, error) {
	// Traversal runs on LIGHT references — bodies are unbounded and
	// irrelevant to blocker resolution; the chosen issue loads in full
	// only once the walk settles on it.
	rows, err := tx.Query(`
		SELECT i.id, i.status, i.assignee FROM issues i
		JOIN projects p ON p.id = i.project
		WHERE i.assignee = ? AND i.status = 'open' AND p.archived_at IS NULL
		ORDER BY i.assigned_at, i.number`, identity)
	if err != nil {
		return nil, fmt.Errorf("pop candidates for %s: %w", identity, err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []walkRef
	for rows.Next() {
		var c walkRef
		if err := rows.Scan(&c.id, &c.status, &c.assignee); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	w := &blockerWalk{tx: tx, identity: identity, memo: map[string]walkResult{}, onPath: map[string]bool{}}
	for _, candidate := range candidates {
		res, err := w.resolve(candidate)
		if err != nil {
			return nil, err
		}
		if res.claimID != "" {
			claimed, err := Get(tx, res.claimID)
			if err != nil {
				return nil, err
			}
			return &claimed, nil
		}
	}
	return nil, nil
}

// walkRef is the traversal's light view of an issue — never the body.
type walkRef struct {
	id       string
	status   string
	assignee *string
}

// blockerWalk memoizes each node's deepest workable claim — correct on
// converging DAGs, where a shared descendant reached first through a
// short path must still contribute its full depth to a longer path.
// Blocking cycles are rejected at relation creation; onPath guards
// termination against corrupted data only.
type blockerWalk struct {
	tx       *sql.Tx
	identity string
	memo     map[string]walkResult
	onPath   map[string]bool
}

type walkResult struct {
	claimID string
	depth   int
}

// resolve computes what to claim for a candidate: itself when
// unblocked, the DEEPEST open self-assigned blocker across every
// branch when the whole blocker set is self-owned, empty when any live
// blocker is external or unworkable. Depth is the chain length to the
// returned claim; ties break toward the lower display number via the
// ordered blocker query.
func (w *blockerWalk) resolve(candidate walkRef) (walkResult, error) {
	if cached, ok := w.memo[candidate.id]; ok {
		return cached, nil
	}
	if w.onPath[candidate.id] {
		// Only reachable through corrupted data — cycles reject at
		// creation. Treat as unworkable rather than recurse forever.
		return walkResult{}, nil
	}
	w.onPath[candidate.id] = true
	defer delete(w.onPath, candidate.id)
	tx, identity := w.tx, w.identity
	rows, err := tx.Query(`
		SELECT b.id, b.status, b.assignee, (bp.archived_at IS NOT NULL) FROM issue_relations r
		JOIN issues b ON b.id = r.from_issue
		JOIN projects bp ON bp.id = b.project
		WHERE r.kind = 'blocks' AND r.to_issue = ? AND b.status != 'complete'
		ORDER BY b.number`, candidate.id)
	if err != nil {
		return walkResult{}, fmt.Errorf("blockers of %s: %w", candidate.id, err)
	}
	defer func() { _ = rows.Close() }()
	type blockerRow struct {
		ref      walkRef
		archived bool
	}
	var blockers []blockerRow
	for rows.Next() {
		var b blockerRow
		if err := rows.Scan(&b.ref.id, &b.ref.status, &b.ref.assignee, &b.archived); err != nil {
			return walkResult{}, fmt.Errorf("scan blocker: %w", err)
		}
		blockers = append(blockers, b)
	}
	if err := rows.Err(); err != nil {
		return walkResult{}, fmt.Errorf("iterate blockers: %w", err)
	}
	result := walkResult{}
	switch {
	case len(blockers) == 0:
		result = walkResult{claimID: candidate.id, depth: 0}
	default:
		eligible := true
		for _, b := range blockers {
			if b.archived || b.ref.assignee == nil || *b.ref.assignee != identity || b.ref.status != StatusOpen {
				// Live work assigned elsewhere, unworkable, or frozen
				// in an archived project blocks the whole chain — an
				// archived project's issue must never be claimed.
				eligible = false
				break
			}
		}
		if eligible {
			// Walk EVERY branch and claim the deepest — a shallow
			// branch must not shadow deeper prerequisite work. ANY
			// unworkable branch (an external or frozen block deeper
			// down) poisons the whole candidate, exactly as a direct
			// external blocker does: its prerequisites cannot all be
			// worked by this identity, so an older workable fallback
			// must win instead.
			bestDepth := -1
			bestID := ""
			poisoned := false
			for _, b := range blockers {
				sub, err := w.resolve(b.ref)
				if err != nil {
					return walkResult{}, err
				}
				if sub.claimID == "" {
					poisoned = true
					break
				}
				if sub.depth > bestDepth {
					bestID, bestDepth = sub.claimID, sub.depth
				}
			}
			// `bestID != ""` used to guard this too, and it was redundant:
			// this branch only runs with at least one blocker, so the loop
			// above always executes, and an unpoisoned pass means every
			// sub-claim was non-empty. Verified rather than argued — with
			// the conjunct dropped the whole suite stays green, while
			// ignoring `poisoned` fails
			// TestAPoisonedCandidateYieldsToTheOlderFallback. Finding that
			// out is what exposed the two gaps that test now fills.
			if !poisoned {
				result = walkResult{claimID: bestID, depth: bestDepth + 1}
			}
		}
	}
	w.memo[candidate.id] = result
	return result, nil
}

// CompleteAncestors returns id's complete ancestors, nearest first —
// the set the reopen cascade flips (AC-parent-reopen-cascade).
func CompleteAncestors(tx *sql.Tx, id string) ([]string, error) {
	chain, err := Ancestors(tx, id)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range chain {
		var status string
		if err := tx.QueryRow(`SELECT status FROM issues WHERE id = ?`, a).Scan(&status); err != nil {
			return nil, fmt.Errorf("status of ancestor %s: %w", a, err)
		}
		if status == StatusComplete {
			out = append(out, a)
		}
	}
	return out, nil
}

// newUUIDv7 returns an RFC 9562 UUIDv7 (CON-uuid-keys). Duplicated
// per-package; MOD-issues imports only its declared siblings.
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
