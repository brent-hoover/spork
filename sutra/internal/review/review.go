// Package review — see MOD-review in avspec.yaml. Owns reviews, their
// immutable per-revision submissions, verdicts, atomic approval
// consumption, and the close-authorization stamp. Boundaries allow only
// identity and events, so the api layer supplies project facts
// (repo_path, default_branch) and owns transactions; git resolution
// lives here because the pinned deliverable is this module's rule.
package review

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Review mirrors the contract's Review schema; optional fields are
// omitted when absent, never null.
type Review struct {
	ID                 string       `json:"id"`
	Issue              string       `json:"issue"`
	Branch             *string      `json:"branch,omitempty"`
	Commit             *string      `json:"commit,omitempty"`
	DocVersion         *string      `json:"doc_version,omitempty"`
	Session            *string      `json:"session,omitempty"`
	Summary            *string      `json:"summary,omitempty"`
	Author             string       `json:"author"`
	State              string       `json:"state"`
	Revision           int64        `json:"revision"`
	LatestVerdictEvent *string      `json:"latest_verdict_event,omitempty"`
	Consumed           *string      `json:"consumed,omitempty"`
	ConsumedRevision   *int64       `json:"consumed_revision,omitempty"`
	CloseUsed          *string      `json:"close_used,omitempty"`
	Submissions        []Submission `json:"submissions"`
	Created            string       `json:"created"`
}

// Submission mirrors ReviewSubmission — immutable, one per revision.
type Submission struct {
	ID         string  `json:"id"`
	Review     string  `json:"review"`
	Revision   int64   `json:"revision"`
	Branch     *string `json:"branch,omitempty"`
	Commit     *string `json:"commit,omitempty"`
	BaseCommit *string `json:"base_commit,omitempty"`
	DocVersion *string `json:"doc_version,omitempty"`
	Session    *string `json:"session,omitempty"`
	Created    string  `json:"created"`
}

// States of a review.
const (
	StateOpen             = "open"
	StateChangesRequested = "changes-requested"
	StateApproved         = "approved"
)

// NotFoundError reports an id that names no review.
type NotFoundError struct{ ID string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("review %s not found", e.ID) }

// ConflictError is a review-domain conflict carrying its contract code.
type ConflictError struct {
	Code    string
	Message string
}

func (e *ConflictError) Error() string { return e.Message }

// GitError reports an unresolvable repo/ref at submission time — the
// contract's 409.
type GitError struct{ Message string }

func (e *GitError) Error() string { return e.Message }

// Migrate creates the review tables.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS reviews (
			id                   TEXT PRIMARY KEY,
			issue                TEXT NOT NULL,
			author               TEXT NOT NULL,
			state                TEXT NOT NULL DEFAULT 'open'
				CHECK (state IN ('open', 'changes-requested', 'approved')),
			revision             INTEGER NOT NULL DEFAULT 1,
			session              TEXT,
			summary              TEXT,
			branch               TEXT,
			commit_sha           TEXT,
			doc_version          TEXT,
			latest_verdict_event TEXT,
			consumed             TEXT,
			consumed_revision    INTEGER,
			close_used           TEXT,
			created              TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS review_submissions (
			id          TEXT PRIMARY KEY,
			review      TEXT NOT NULL REFERENCES reviews(id),
			revision    INTEGER NOT NULL,
			branch      TEXT,
			commit_sha  TEXT,
			base_commit TEXT,
			doc_version TEXT,
			session     TEXT,
			created     TEXT NOT NULL,
			UNIQUE (review, revision)
		);
	`)
	if err != nil {
		return fmt.Errorf("migrate reviews: %w", err)
	}
	return nil
}

// Deliverable is the exactly-one shape shared by create and resubmit.
type Deliverable struct {
	Branch     *string
	Commit     *string
	DocVersion *string
	Session    *string
}

// Repo carries the project facts the api layer resolves for code
// deliverables (review cannot import projects).
type Repo struct {
	Path          string // empty means unset
	DefaultBranch string
}

// Fences carries the caller's optional creation-time expectations.
type Fences struct {
	ExpectedBaseCommit  *string
	ExpectedDefaultHead *string
}

// ResolveCode pins a code deliverable: base_commit is the merge base of
// commit with the project's default branch, resolved here and never
// client-supplied; the optional fences validate against the resolved
// base and the CURRENT default head. Runs git — call it before the
// database transaction opens.
func ResolveCode(repo Repo, commit string, f Fences) (baseCommit string, err error) {
	if repo.Path == "" {
		return "", &GitError{Message: "project has no repo_path; code deliverables cannot be resolved"}
	}
	head, err := gitOut(repo.Path, "rev-parse", "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		return "", &GitError{Message: fmt.Sprintf("default branch %q unresolvable: %v", repo.DefaultBranch, err)}
	}
	if f.ExpectedDefaultHead != nil && *f.ExpectedDefaultHead != head {
		return "", &ConflictError{Code: "expected-default-head-mismatch",
			Message: fmt.Sprintf("default head is %s, expected %s", head, *f.ExpectedDefaultHead)}
	}
	base, err := gitOut(repo.Path, "merge-base", commit, "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		return "", &GitError{Message: fmt.Sprintf("merge base of %s unresolvable: %v", commit, err)}
	}
	if f.ExpectedBaseCommit != nil && *f.ExpectedBaseCommit != base {
		return "", &ConflictError{Code: "expected-base-mismatch",
			Message: fmt.Sprintf("resolved base is %s, expected %s", base, *f.ExpectedBaseCommit)}
	}
	return base, nil
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Create inserts a review in state open at revision 1 with its first
// submission. baseCommit comes from ResolveCode for code deliverables
// and is empty for doc deliverables.
func Create(tx *sql.Tx, issue, author string, d Deliverable, summary *string, baseCommit string) (Review, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r := Review{
		ID: newUUIDv7(), Issue: issue, Author: author, State: StateOpen,
		Revision: 1, Branch: d.Branch, Commit: d.Commit, DocVersion: d.DocVersion,
		Session: d.Session, Summary: summary, Created: now,
	}
	_, err := tx.Exec(`
		INSERT INTO reviews (id, issue, author, state, revision, session, summary, branch, commit_sha, doc_version, created)
		VALUES (?, ?, ?, 'open', 1, ?, ?, ?, ?, ?, ?)`,
		r.ID, issue, author, d.Session, summary, d.Branch, d.Commit, d.DocVersion, now)
	if err != nil {
		return Review{}, fmt.Errorf("insert review: %w", err)
	}
	sub, err := appendSubmission(tx, r.ID, 1, d, baseCommit, now)
	if err != nil {
		return Review{}, err
	}
	r.Submissions = []Submission{sub}
	return r, nil
}

func appendSubmission(tx *sql.Tx, reviewID string, revision int64, d Deliverable, baseCommit, now string) (Submission, error) {
	sub := Submission{
		ID: newUUIDv7(), Review: reviewID, Revision: revision,
		Branch: d.Branch, Commit: d.Commit, DocVersion: d.DocVersion,
		Session: d.Session, Created: now,
	}
	if baseCommit != "" {
		sub.BaseCommit = &baseCommit
	}
	_, err := tx.Exec(`
		INSERT INTO review_submissions (id, review, revision, branch, commit_sha, base_commit, doc_version, session, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.ID, reviewID, revision, d.Branch, d.Commit, sub.BaseCommit, d.DocVersion, d.Session, now)
	if err != nil {
		return Submission{}, fmt.Errorf("insert submission: %w", err)
	}
	return sub, nil
}

// Get returns one review with its submissions.
func Get(tx *sql.Tx, id string) (Review, error) {
	r, err := scanReview(tx.QueryRow(`
		SELECT id, issue, author, state, revision, session, summary, branch, commit_sha, doc_version,
		       latest_verdict_event, consumed, consumed_revision, close_used, created
		FROM reviews WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Review{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Review{}, fmt.Errorf("get review %s: %w", id, err)
	}
	rows, err := tx.Query(`
		SELECT id, review, revision, branch, commit_sha, base_commit, doc_version, session, created
		FROM review_submissions WHERE review = ? ORDER BY revision`, id)
	if err != nil {
		return Review{}, fmt.Errorf("submissions of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s Submission
		if err := rows.Scan(&s.ID, &s.Review, &s.Revision, &s.Branch, &s.Commit, &s.BaseCommit, &s.DocVersion, &s.Session, &s.Created); err != nil {
			return Review{}, fmt.Errorf("scan submission: %w", err)
		}
		r.Submissions = append(r.Submissions, s)
	}
	if err := rows.Err(); err != nil {
		return Review{}, fmt.Errorf("iterate submissions: %w", err)
	}
	return r, nil
}

type rowScanner interface{ Scan(...any) error }

func scanReview(row rowScanner) (Review, error) {
	var r Review
	err := row.Scan(&r.ID, &r.Issue, &r.Author, &r.State, &r.Revision, &r.Session, &r.Summary,
		&r.Branch, &r.Commit, &r.DocVersion, &r.LatestVerdictEvent, &r.Consumed, &r.ConsumedRevision,
		&r.CloseUsed, &r.Created)
	if err != nil {
		return Review{}, err
	}
	r.Submissions = []Submission{}
	return r, nil
}

// List returns reviews filtered by issue, state, and session — a
// session matches when ANY submission carries it (AC-search-session).
func List(tx *sql.Tx, issue, state, session string) ([]Review, error) {
	query := `SELECT DISTINCT r.id FROM reviews r`
	args := []any{}
	var where []string
	if session != "" {
		query += ` JOIN review_submissions s ON s.review = r.id`
		where = append(where, `s.session = ?`)
		args = append(args, session)
	}
	if issue != "" {
		where = append(where, `r.issue = ?`)
		args = append(args, issue)
	}
	if state != "" {
		where = append(where, `r.state = ?`)
		args = append(args, state)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY r.id`
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan review id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reviews: %w", err)
	}
	out := []Review{}
	for _, id := range ids {
		r, err := Get(tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// SetVerdict applies approved or changes-requested to the revision the
// reviewer actually saw (AC-review-stale-guard): a verdict naming a
// revision superseded by a resubmission is rejected, never applied to
// unseen content. A consumed review's verdict is frozen — conflict, no
// mutation (AC-review-consume). latest_verdict_event is replaced with
// the given event id.
func SetVerdict(tx *sql.Tx, id, state string, revision int64, verdictEvent string) (Review, error) {
	r, err := Get(tx, id)
	if err != nil {
		return Review{}, err
	}
	if r.Consumed != nil {
		return Review{}, &ConflictError{Code: "review-consumed",
			Message: fmt.Sprintf("review %s is consumed; its verdict is frozen", id)}
	}
	if r.Revision != revision {
		return Review{}, &ConflictError{Code: "expected-revision-mismatch",
			Message: fmt.Sprintf("verdict names revision %d but review %s is at %d — the reviewer did not see this content", revision, id, r.Revision)}
	}
	if _, err := tx.Exec(`UPDATE reviews SET state = ?, latest_verdict_event = ? WHERE id = ?`,
		state, verdictEvent, id); err != nil {
		return Review{}, fmt.Errorf("set verdict on %s: %w", id, err)
	}
	return Get(tx, id)
}

// Consume atomically stamps approval consumption: state approved,
// revision and latest verdict event both matching — the verdict-ABA
// fence — and not already consumed. Distinct attempts against a
// consumed approval conflict, so two subscribers can never both act.
func Consume(tx *sql.Tx, id string, expectedRevision int64, expectedVerdictEvent string) (Review, error) {
	r, err := Get(tx, id)
	if err != nil {
		return Review{}, err
	}
	if r.Consumed != nil {
		return Review{}, &ConflictError{Code: "review-consumed",
			Message: fmt.Sprintf("review %s approval already consumed at revision %d", id, *r.ConsumedRevision)}
	}
	if r.State != StateApproved {
		return Review{}, &ConflictError{Code: "review-not-approved",
			Message: fmt.Sprintf("review %s is %s, not approved", id, r.State)}
	}
	if r.Revision != expectedRevision {
		return Review{}, &ConflictError{Code: "expected-revision-mismatch",
			Message: fmt.Sprintf("review %s is at revision %d, expected %d", id, r.Revision, expectedRevision)}
	}
	if r.LatestVerdictEvent == nil || *r.LatestVerdictEvent != expectedVerdictEvent {
		return Review{}, &ConflictError{Code: "expected-verdict-event-mismatch",
			Message: fmt.Sprintf("review %s verdict event moved", id)}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.Exec(`
		UPDATE reviews SET consumed = ?, consumed_revision = revision
		WHERE id = ? AND consumed IS NULL AND state = 'approved' AND revision = ?`,
		now, id, expectedRevision)
	if err != nil {
		return Review{}, fmt.Errorf("consume %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return Review{}, &ConflictError{Code: "review-consumed", Message: fmt.Sprintf("review %s consumption raced", id)}
	}
	return Get(tx, id)
}

// Resubmit replaces the deliverable from state changes-requested at the
// expected revision AND the expected latest verdict event — stale
// rework can never replace a deliverable without addressing the latest
// feedback. revision++, new immutable submission, state open.
func Resubmit(tx *sql.Tx, id string, expectedRevision int64, expectedVerdictEvent string, d Deliverable, baseCommit string) (Review, error) {
	r, err := Get(tx, id)
	if err != nil {
		return Review{}, err
	}
	if r.State != StateChangesRequested {
		return Review{}, &ConflictError{Code: "expected-status-mismatch",
			Message: fmt.Sprintf("review %s is %s; only changes-requested resubmits", id, r.State)}
	}
	if r.Revision != expectedRevision {
		return Review{}, &ConflictError{Code: "expected-revision-mismatch",
			Message: fmt.Sprintf("review %s is at revision %d, expected %d", id, r.Revision, expectedRevision)}
	}
	if r.LatestVerdictEvent == nil || *r.LatestVerdictEvent != expectedVerdictEvent {
		return Review{}, &ConflictError{Code: "expected-verdict-event-mismatch",
			Message: fmt.Sprintf("review %s verdict event moved; rework must answer the latest feedback", id)}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	next := r.Revision + 1
	if _, err := tx.Exec(`
		UPDATE reviews SET state = 'open', revision = ?, branch = ?, commit_sha = ?, doc_version = ?, session = ?
		WHERE id = ?`, next, d.Branch, d.Commit, d.DocVersion, d.Session, id); err != nil {
		return Review{}, fmt.Errorf("resubmit %s: %w", id, err)
	}
	if _, err := appendSubmission(tx, id, next, d, baseCommit, now); err != nil {
		return Review{}, err
	}
	return Get(tx, id)
}

// SpendForClose validates and stamps close authorization atomically:
// the review must belong to the issue, be approved at exactly the named
// revision with the named latest verdict event, and never close-used.
// Consumption fields are stamped too when absent — close_used implies
// them (contract). Returns the conflict the contract names otherwise.
func SpendForClose(tx *sql.Tx, id, issue string, revision int64, verdictEvent string) (Review, error) {
	r, err := Get(tx, id)
	if err != nil {
		var notFound *NotFoundError
		if errors.As(err, &notFound) {
			// An unknown review id on a close is the ownership failure,
			// not a 404 — the contract's 409 (AC-close-approved).
			return Review{}, &ConflictError{Code: "missing-approval",
				Message: fmt.Sprintf("review %s does not exist", id)}
		}
		return Review{}, err
	}
	if r.Issue != issue {
		return Review{}, &ConflictError{Code: "missing-approval",
			Message: fmt.Sprintf("review %s does not belong to issue %s", id, issue)}
	}
	if r.CloseUsed != nil {
		return Review{}, &ConflictError{Code: "review-close-used",
			Message: fmt.Sprintf("review %s already authorized a close", id)}
	}
	if r.State != StateApproved {
		return Review{}, &ConflictError{Code: "review-not-approved",
			Message: fmt.Sprintf("review %s is %s, not approved", id, r.State)}
	}
	if r.Revision != revision {
		return Review{}, &ConflictError{Code: "expected-revision-mismatch",
			Message: fmt.Sprintf("review %s is at revision %d, close names %d", id, r.Revision, revision)}
	}
	if r.LatestVerdictEvent == nil || *r.LatestVerdictEvent != verdictEvent {
		return Review{}, &ConflictError{Code: "expected-verdict-event-mismatch",
			Message: fmt.Sprintf("review %s verdict event moved since the close was prepared", id)}
	}
	if r.ConsumedRevision != nil && *r.ConsumedRevision != r.Revision {
		return Review{}, &ConflictError{Code: "review-consumed",
			Message: fmt.Sprintf("review %s was consumed at revision %d, not current", id, *r.ConsumedRevision)}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.Exec(`
		UPDATE reviews SET close_used = ?,
			consumed = COALESCE(consumed, ?),
			consumed_revision = COALESCE(consumed_revision, revision)
		WHERE id = ? AND close_used IS NULL AND state = 'approved' AND revision = ?`,
		now, now, id, revision)
	if err != nil {
		return Review{}, fmt.Errorf("spend %s for close: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return Review{}, &ConflictError{Code: "review-close-used", Message: fmt.Sprintf("review %s close raced", id)}
	}
	return Get(tx, id)
}

// newUUIDv7 returns an RFC 9562 UUIDv7 (CON-uuid-keys). Duplicated
// per-package; MOD-review imports only identity and events.
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
