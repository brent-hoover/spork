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

// Issue mirrors the contract's Issue schema (labels live with the
// labels endpoints and join at the API layer once implemented).
type Issue struct {
	ID              string  `json:"id"`
	Number          int64   `json:"number"`
	Title           string  `json:"title"`
	Body            *string `json:"body,omitempty"`
	Status          string  `json:"status"`
	Project         string  `json:"project"`
	Assignee        *string `json:"assignee,omitempty"`
	Created         string  `json:"created"`
	Updated         string  `json:"updated"`
	SubtreeRevision int64   `json:"subtree_revision"`
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
	return nil
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
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
	return i, nil
}

// Filters narrows List. Text search and label filtering arrive with
// the search and labels modules.
type Filters struct {
	Number   *int64
	Statuses []string
	Assignee string
}

// List returns a project's issues, filterable by number, status, and
// assignee.
func List(tx *sql.Tx, project string, f Filters) ([]Issue, error) {
	query := `SELECT ` + issueColumns + ` FROM issues WHERE project = ?`
	args := []any{project}
	if f.Number != nil {
		query += ` AND number = ?`
		args = append(args, *f.Number)
	}
	if len(f.Statuses) > 0 {
		query += ` AND status IN (?` + strings.Repeat(",?", len(f.Statuses)-1) + `)`
		for _, s := range f.Statuses {
			args = append(args, s)
		}
	}
	if f.Assignee != "" {
		query += ` AND assignee = ?`
		args = append(args, f.Assignee)
	}
	query += ` ORDER BY number`
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Issue{}
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("scan issue: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issues: %w", err)
	}
	return out, nil
}

// Update rewrites title/body and stamps updated (AC-issue-update).
func Update(tx *sql.Tx, id string, title, body *string) (Issue, error) {
	if _, err := Get(tx, id); err != nil {
		return Issue{}, err
	}
	sets, args := []string{"updated = ?"}, []any{time.Now().UTC().Format(time.RFC3339Nano)}
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
		status, time.Now().UTC().Format(time.RFC3339Nano), id)
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
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
	rows, err := tx.Query(`
		SELECT `+prefixedIssueColumns("i")+` FROM issues i
		JOIN projects p ON p.id = i.project
		WHERE i.assignee = ? AND i.status = 'open' AND p.archived_at IS NULL
		ORDER BY i.assigned_at, i.number`, identity)
	if err != nil {
		return nil, fmt.Errorf("pop candidates for %s: %w", identity, err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	for _, candidate := range candidates {
		claim, err := walkBlockers(tx, identity, candidate, map[string]bool{})
		if err != nil {
			return nil, err
		}
		if claim != nil {
			return claim, nil
		}
	}
	return nil, nil
}

// walkBlockers resolves what to claim for a candidate: itself when
// unblocked, the deepest open self-assigned blocker when the chain is
// self-owned, nil when any live blocker is external or unworkable.
func walkBlockers(tx *sql.Tx, identity string, candidate Issue, visited map[string]bool) (*Issue, error) {
	if visited[candidate.ID] {
		return nil, nil
	}
	visited[candidate.ID] = true
	rows, err := tx.Query(`
		SELECT `+prefixedIssueColumns("b")+`, (bp.archived_at IS NOT NULL) FROM issue_relations r
		JOIN issues b ON b.id = r.from_issue
		JOIN projects bp ON bp.id = b.project
		WHERE r.kind = 'blocks' AND r.to_issue = ? AND b.status != 'complete'
		ORDER BY b.number`, candidate.ID)
	if err != nil {
		return nil, fmt.Errorf("blockers of %s: %w", candidate.ID, err)
	}
	defer func() { _ = rows.Close() }()
	type blockerRow struct {
		issue    Issue
		archived bool
	}
	var blockers []blockerRow
	for rows.Next() {
		var b Issue
		var archived bool
		cols := scanTargets(&b)
		cols = append(cols, &archived)
		if err := rows.Scan(cols...); err != nil {
			return nil, fmt.Errorf("scan blocker: %w", err)
		}
		blockers = append(blockers, blockerRow{issue: b, archived: archived})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate blockers: %w", err)
	}
	if len(blockers) == 0 {
		return &candidate, nil
	}
	for _, b := range blockers {
		if b.archived || b.issue.Assignee == nil || *b.issue.Assignee != identity || b.issue.Status != StatusOpen {
			// Live work assigned elsewhere, unworkable, or frozen in an
			// archived project blocks the whole chain for this identity
			// — an archived project's issue must never be claimed.
			return nil, nil
		}
	}
	// Every live blocker is self-assigned and open: work the deepest.
	return walkBlockers(tx, identity, blockers[0].issue, visited)
}

// scanTargets returns scan destinations matching issueColumns order.
func scanTargets(i *Issue) []any {
	return []any{&i.ID, &i.Number, &i.Title, &i.Body, &i.Status, &i.Project, &i.Assignee, &i.Created, &i.Updated, &i.SubtreeRevision}
}

func prefixedIssueColumns(alias string) string {
	cols := strings.Split(issueColumns, ", ")
	for i, c := range cols {
		cols[i] = alias + "." + c
	}
	return strings.Join(cols, ", ")
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
	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
