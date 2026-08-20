// Package review — see MOD-review in avspec.yaml. Owns reviews, their
// immutable per-revision submissions, verdicts, atomic approval
// consumption, and the close-authorization stamp. Boundaries allow only
// identity and events, so the api layer supplies project facts
// (repo_path, default_branch) and owns transactions; git resolution
// lives here because the pinned deliverable is this module's rule.
package review

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// tsLayout is RFC 3339 with FIXED-WIDTH nanoseconds. time.RFC3339Nano
// trims trailing zeros, so its output does not sort lexically: with
// "…992647Z" against "…9926475Z", 'Z' > '5' and the earlier instant
// compares greater. Timestamps are stored and ordered as TEXT, so the
// format IS the ordering (review 1898).
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

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
// Content holds the diff exactly as validated at submission time; it is
// storage, not wire shape (the deliverable endpoint serves it).
type Submission struct {
	ID         string  `json:"id"`
	Review     string  `json:"review"`
	Revision   int64   `json:"revision"`
	Branch     *string `json:"branch,omitempty"`
	Commit     *string `json:"commit,omitempty"`
	BaseCommit *string `json:"base_commit,omitempty"`
	DocVersion *string `json:"doc_version,omitempty"`
	Session    *string `json:"session,omitempty"`
	// Content is the durable deliverable copy. Ordinary review reads
	// leave it nil (metadata queries exclude the column), so it is
	// omitted from metadata responses; the deliverable endpoint fetches
	// one submission's content directly, and export payloads carry it —
	// it is the ONLY durable copy once git objects go unreachable.
	Content *string `json:"content,omitempty"`
	Created string  `json:"created"`
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
			content     TEXT,
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
	head, err := resolveHead(repo, 10*time.Second)
	if err != nil {
		return "", err
	}
	if f.ExpectedDefaultHead != nil && *f.ExpectedDefaultHead != head {
		return "", &ConflictError{Code: "expected-default-head-mismatch",
			Message: fmt.Sprintf("default head is %s, expected %s", head, *f.ExpectedDefaultHead)}
	}
	// The merge base is computed against the SAME head the fence saw —
	// the immutable object id, not the mutable ref, so a branch advance
	// between commands can never split the fence from the pin.
	base, err := gitOut(repo.Path, "merge-base", commit, head)
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
	return gitOutTimeout(dir, 10*time.Second, args...)
}

// resolveHead resolves the default branch's EXACT ref via
// show-ref --verify — never rev-parse, whose revision syntax would let
// a value like "main~1" resolve to an ancestor instead of failing.
func resolveHead(repo Repo, timeout time.Duration) (string, error) {
	head, err := gitOutTimeout(repo.Path, timeout, "show-ref", "--verify", "--hash", "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		return "", &GitError{Message: fmt.Sprintf("default branch %q unresolvable: %v", repo.DefaultBranch, err)}
	}
	return head, nil
}

func gitOutTimeout(dir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Create inserts a review in state open at revision 1 with its first
// submission. baseCommit comes from ResolveCode for code deliverables
// and is empty for doc deliverables.
func Create(tx *sql.Tx, issue, author string, d Deliverable, summary *string, baseCommit string, content *string) (Review, error) {
	now := time.Now().UTC().Format(tsLayout)
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
	sub, err := appendSubmission(tx, r.ID, 1, d, baseCommit, content, now)
	if err != nil {
		return Review{}, err
	}
	r.Submissions = []Submission{sub}
	return r, nil
}

func appendSubmission(tx *sql.Tx, reviewID string, revision int64, d Deliverable, baseCommit string, content *string, now string) (Submission, error) {
	// content goes to the ROW only: metadata responses (create included)
	// stay content-free — the deliverable endpoint and export paths are
	// the only readers, so create responses match GET shapes and the
	// idempotency store never duplicates multi-megabyte diffs.
	sub := Submission{
		ID: newUUIDv7(), Review: reviewID, Revision: revision,
		Branch: d.Branch, Commit: d.Commit, DocVersion: d.DocVersion,
		Session: d.Session, Created: now,
	}
	if baseCommit != "" {
		sub.BaseCommit = &baseCommit
	}
	_, err := tx.Exec(`
		INSERT INTO review_submissions (id, review, revision, branch, commit_sha, base_commit, doc_version, session, content, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.ID, reviewID, revision, d.Branch, d.Commit, sub.BaseCommit, d.DocVersion, d.Session, content, now)
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
	// Metadata only: content is fetched lazily by ContentAt, so review
	// lookups never load the full deliverable history into memory.
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

// GetMeta is Get without the submission history. Callers that stream
// submissions separately (export) must use it: Get accumulates one
// Submission per revision, so loading a review and then streaming the
// same submissions holds that history twice (review 1908).
func GetMeta(tx *sql.Tx, id string) (Review, error) {
	r, err := scanReview(tx.QueryRow(`
		SELECT id, issue, author, state, revision, session, summary, branch, commit_sha, doc_version,
		       latest_verdict_event, consumed, consumed_revision, close_used, created
		FROM reviews WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Review{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Review{}, fmt.Errorf("get review meta %s: %w", id, err)
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

// listQuery builds the shared review-selection predicate, so the full
// listing and the light projection can never drift apart in what they
// match — only in what they carry.
func listQuery(selectClause, issue, state, session string) (string, []any) {
	query := selectClause
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
	return query + ` ORDER BY r.id`, args
}

// Ref is the discovery-light view of a review: identity and the two
// ids callers need to plan with. Summaries are unbounded and Get also
// hydrates the whole submission history, so anything that merely
// collects reviews must use EachRef instead (review 1906).
type Ref struct {
	ID       string
	Issue    string
	Author   string
	Revision int64
}

// RefByID reads one review's bounded projection. Validating a comment
// target needs the issue and the revision, never the summary or the
// submission history Get hydrates (review 1926).
func RefByID(tx *sql.Tx, id string) (Ref, error) {
	var r Ref
	err := tx.QueryRow(`SELECT id, issue, author, revision FROM reviews WHERE id = ?`, id).
		Scan(&r.ID, &r.Issue, &r.Author, &r.Revision)
	if err == sql.ErrNoRows {
		return Ref{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Ref{}, fmt.Errorf("review ref %s: %w", id, err)
	}
	return r, nil
}

// EachRef streams matching review refs, selected by the same
// predicates as ListEach but projecting only bounded columns.
func EachRef(tx *sql.Tx, issue, state, session string, fn func(Ref) error) error {
	query, args := listQuery(`SELECT DISTINCT r.id, r.issue, r.author, r.revision FROM reviews r`, issue, state, session)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return fmt.Errorf("list review refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.ID, &r.Issue, &r.Author, &r.Revision); err != nil {
			return fmt.Errorf("scan review ref: %w", err)
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	// Wrapped like ListEach four lines down. A bare rows.Err() reaches the
	// caller as a driver string with no indication of which cursor broke.
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate review refs: %w", err)
	}
	return nil
}

// ListEach streams matching reviews one at a time — summaries are
// unbounded, so wire-serving listings never accumulate them.
func ListEach(tx *sql.Tx, issue, state, session string, fn func(Review) error) error {
	query, args := listQuery(`SELECT DISTINCT r.id FROM reviews r`, issue, state, session)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return fmt.Errorf("list reviews: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan review id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate reviews: %w", err)
	}
	_ = rows.Close()
	for _, id := range ids {
		r, err := Get(tx, id)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
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
	now := time.Now().UTC().Format(tsLayout)
	res, err := tx.Exec(`
		UPDATE reviews SET consumed = ?, consumed_revision = revision
		WHERE id = ? AND consumed IS NULL AND state = 'approved' AND revision = ?`,
		now, id, expectedRevision)
	if err != nil {
		return Review{}, fmt.Errorf("consume %s: %w", id, err)
	}
	// An unreadable row count is UNKNOWN, not zero. Folding it into the
	// raced-conflict answer made a transient database failure a SETTLED
	// 409, recorded against the idempotency key forever — so an approval
	// that was never consumed became permanently unconsumable. The count
	// failing is a 5xx a retry can complete; only a genuine zero is a race.
	n, err := res.RowsAffected()
	if err != nil {
		return Review{}, fmt.Errorf("consume %s: row count unavailable: %w", id, err)
	}
	if n == 0 {
		return Review{}, &ConflictError{Code: "review-consumed", Message: fmt.Sprintf("review %s consumption raced", id)}
	}
	return Get(tx, id)
}

// Resubmit replaces the deliverable from state changes-requested at the
// expected revision AND the expected latest verdict event — stale
// rework can never replace a deliverable without addressing the latest
// feedback. revision++, new immutable submission, state open.
func Resubmit(tx *sql.Tx, id string, expectedRevision int64, expectedVerdictEvent string, d Deliverable, summary *string, baseCommit string, content *string) (Review, error) {
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
	now := time.Now().UTC().Format(tsLayout)
	next := r.Revision + 1
	if _, err := tx.Exec(`
		UPDATE reviews SET state = 'open', revision = ?, branch = ?, commit_sha = ?, doc_version = ?, session = ?,
			summary = COALESCE(?, summary)
		WHERE id = ?`, next, d.Branch, d.Commit, d.DocVersion, d.Session, summary, id); err != nil {
		return Review{}, fmt.Errorf("resubmit %s: %w", id, err)
	}
	if _, err := appendSubmission(tx, id, next, d, baseCommit, content, now); err != nil {
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
	now := time.Now().UTC().Format(tsLayout)
	res, err := tx.Exec(`
		UPDATE reviews SET close_used = ?,
			consumed = COALESCE(consumed, ?),
			consumed_revision = COALESCE(consumed_revision, revision)
		WHERE id = ? AND close_used IS NULL AND state = 'approved' AND revision = ?`,
		now, now, id, revision)
	if err != nil {
		return Review{}, fmt.Errorf("spend %s for close: %w", id, err)
	}
	// Same distinction as Consume: an unknown count must not settle as a
	// close-used conflict, which would strand the close permanently.
	n, err := res.RowsAffected()
	if err != nil {
		return Review{}, fmt.Errorf("spend %s for close: row count unavailable: %w", id, err)
	}
	if n == 0 {
		return Review{}, &ConflictError{Code: "review-close-used", Message: fmt.Sprintf("review %s close raced", id)}
	}
	return Get(tx, id)
}

// MaxDiffBytes bounds deliverable content: a diff larger than this is
// rejected rather than buffered into memory. Variable for tests.
var MaxDiffBytes int64 = 10 << 20

// Diff resolves a code submission's complete pinned content: the diff
// between its immutable base_commit and commit, computed from the
// pinned object ids — never recomputed against the current default
// branch (AC-review-web). Repository-configured helpers are disabled
// (--no-ext-diff --no-textconv --no-color): the pinned patch is the
// bytes git produces, never a transformed rendering and never a
// repo-configured command execution. Output is returned untrimmed and
// bounded by MaxDiffBytes.
func Diff(repoPath, baseCommit, commit string) (string, error) {
	if repoPath == "" {
		return "", &GitError{Message: "project has no repo_path; the pinned diff cannot be resolved"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath,
		"-c", "diff.external=", "-c", "diff.submodule=short",
		"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--binary",
		"--ignore-submodules=none",
		baseCommit, commit)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", &GitError{Message: fmt.Sprintf("pinned diff %s..%s: %v", baseCommit, commit, err)}
	}
	if err := cmd.Start(); err != nil {
		return "", &GitError{Message: fmt.Sprintf("pinned diff %s..%s: %v", baseCommit, commit, err)}
	}
	limited := io.LimitReader(stdout, MaxDiffBytes+1)
	raw, readErr := io.ReadAll(limited)
	if int64(len(raw)) > MaxDiffBytes {
		// Overflow detected: kill git immediately instead of letting it
		// block on a full pipe until the timeout — oversized requests
		// must not pin processes or handler capacity.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", &GitError{Message: fmt.Sprintf("pinned diff %s..%s exceeds the %d-byte limit", baseCommit, commit, MaxDiffBytes)}
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return "", &GitError{Message: fmt.Sprintf("pinned diff %s..%s: %v", baseCommit, commit, readErr)}
	}
	if waitErr != nil {
		return "", &GitError{Message: fmt.Sprintf("pinned diff %s..%s unresolvable: %v", baseCommit, commit, waitErr)}
	}
	// JSON transport replaces invalid UTF-8 with U+FFFD, silently
	// corrupting the content approval covers. Binary changes already
	// travel as ASCII (--binary); a text-classified file with a
	// non-UTF-8 encoding is unrenderable and rejects at submission.
	if !utf8.Valid(raw) {
		return "", &GitError{Message: fmt.Sprintf("pinned diff %s..%s contains non-UTF-8 text content and cannot be rendered faithfully", baseCommit, commit)}
	}
	return string(raw), nil
}

// Render produces and validates the pinned diff at submission time —
// within MaxDiffBytes, valid UTF-8, resolvable. The returned content is
// STORED with the submission and served verbatim thereafter: the
// deliverable is immutable by construction, beyond the reach of later
// repository configuration, attribute changes, ref deletion, or
// garbage collection. A review whose content nobody could ever open is
// never created, let alone approved.
func Render(repoPath, baseCommit, commit string) (string, error) {
	return Diff(repoPath, baseCommit, commit)
}

// RevalidateFences re-checks BOTH submission fences with bounded git
// (500ms per command) at the TAIL of the prepare stage — no database
// lock held. The head is resolved once; when a base fence is supplied
// the merge base is recomputed against that same immutable head sha.
// This is the tightest observation an implementation can make: the
// repository is an external store, so fences carry
// observed-at-submission semantics (they reject stale caller
// knowledge) and nothing serializes against concurrent pushes — see
// the contract's expected_base_commit/expected_default_head.
//
// A submission that pins nothing has nothing to recheck, and this is
// the one place that decides so — every caller hands its fences over
// unconditionally. Returning before resolveHead is not merely an
// optimization: with no fence supplied there is no expectation any
// repository state could contradict, so consulting git here could only
// invent a failure about a repository the submission never made a
// claim on.
func RevalidateFences(repo Repo, commit string, f Fences) error {
	if f.ExpectedBaseCommit == nil && f.ExpectedDefaultHead == nil {
		return nil
	}
	head, err := resolveHead(repo, 500*time.Millisecond)
	if err != nil {
		return err
	}
	if f.ExpectedDefaultHead != nil && *f.ExpectedDefaultHead != head {
		return &ConflictError{Code: "expected-default-head-mismatch",
			Message: fmt.Sprintf("default head moved to %s after preparation, expected %s", head, *f.ExpectedDefaultHead)}
	}
	if f.ExpectedBaseCommit != nil {
		base, err := gitOutTimeout(repo.Path, 500*time.Millisecond, "merge-base", commit, head)
		if err != nil {
			return &GitError{Message: fmt.Sprintf("merge base of %s unresolvable: %v", commit, err)}
		}
		if *f.ExpectedBaseCommit != base {
			return &ConflictError{Code: "expected-base-mismatch",
				Message: fmt.Sprintf("merge base moved to %s after preparation, expected %s", base, *f.ExpectedBaseCommit)}
		}
	}
	return nil
}

// ContentAt fetches exactly one submission's stored content — the
// deliverable endpoint's single-row read; 0 selects the latest. A nil
// content conflates the two ways there is none: no such row, and a row
// whose content is SQL NULL. That conflation is safe because the caller
// has already established which submission it is asking about, and it
// resolves the NULL case itself: a doc deliverable stores nothing on
// purpose — its immutable version IS the deliverable — so a nil sends
// the caller to that version rather than to an error.
func ContentAt(tx *sql.Tx, reviewID string, revision int64) (*string, error) {
	query := `SELECT content FROM review_submissions WHERE review = ? ORDER BY revision DESC LIMIT 1`
	args := []any{reviewID}
	if revision != 0 {
		query = `SELECT content FROM review_submissions WHERE review = ? AND revision = ?`
		args = []any{reviewID, revision}
	}
	var content *string
	err := tx.QueryRow(query, args...).Scan(&content)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("content of %s r%d: %w", reviewID, revision, err)
	}
	return content, nil
}

// SubmissionAt returns the submission for a revision, or the latest
// when revision is 0.
func SubmissionAt(r Review, revision int64) (Submission, bool) {
	if len(r.Submissions) == 0 {
		return Submission{}, false
	}
	if revision == 0 {
		return r.Submissions[len(r.Submissions)-1], true
	}
	for _, s := range r.Submissions {
		if s.Revision == revision {
			return s, true
		}
	}
	return Submission{}, false
}

// newUUIDv7 returns an RFC 9562 UUIDv7 (CON-uuid-keys). Duplicated
// per-package; MOD-review imports only identity and events.
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
