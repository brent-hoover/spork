package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"sutra/internal/comments"
	"sutra/internal/docs"
	"sutra/internal/events"
	"sutra/internal/issues"
	"sutra/internal/review"
)

func commentErrorFrom(err error) *apiError {
	switch err.(type) {
	case *comments.NotFoundError:
		return &apiError{status: http.StatusNotFound, code: "not-found", message: err.Error()}
	case *comments.ParentAnchorError:
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: err.Error()}
	default:
		return errorFrom(err)
	}
}

// resolveCommentAnchor validates the oneOf (exactly one of issue,
// doc_version, review; review_revision iff review), checks the target
// exists and its project writes, applies the review stale-guard
// (AC-review-stale-guard), and returns the event subject: the issue
// for issue- and review-anchored comments, the document's issue or
// project for doc-version comments.
func (s *server) resolveCommentAnchor(tx *sql.Tx, issue, docVersion, reviewID *string, reviewRevision *int64) (string, *apiError) {
	set := 0
	for _, p := range []*string{issue, docVersion, reviewID} {
		if p != nil {
			set++
		}
	}
	if set != 1 {
		return "", &apiError{status: http.StatusBadRequest, code: "bad-request", message: "exactly one of issue, doc_version, or review is required"}
	}
	if (reviewRevision != nil) != (reviewID != nil) {
		return "", &apiError{status: http.StatusBadRequest, code: "bad-request", message: "review_revision is required exactly when review is set"}
	}
	switch {
	case issue != nil:
		// The issue's project and id, not its body and labels — this
		// runs under the write lock (review 1928).
		target, err := issues.RefByID(tx, *issue)
		if err != nil {
			return "", issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, target.Project); apiErr != nil {
			return "", apiErr
		}
		return target.ID, nil
	case docVersion != nil:
		// Metadata only: this needs the version's document id, and the
		// content is unbounded — loading it here would run under the
		// write lock the idempotency reservation already holds
		// (review 1902).
		version, err := docs.VersionMetaByID(tx, *docVersion)
		if err != nil {
			return "", docErrorFrom(err)
		}
		// Metadata again: this needs the document's project and issue
		// anchor, and docs.Get would read its unbounded title under the
		// write lock (review 1926).
		doc, err := docs.MetaByID(tx, version.Document)
		if err != nil {
			return "", docErrorFrom(err)
		}
		if apiErr := guardWritable(tx, doc.Project); apiErr != nil {
			return "", apiErr
		}
		if doc.Issue != nil {
			return *doc.Issue, nil
		}
		return doc.Project, nil
	default:
		// The bounded projection: validation compares a revision and
		// follows the issue. review.Get would load the unbounded
		// summary AND the whole submission history to do it.
		rev, err := review.RefByID(tx, *reviewID)
		if err != nil {
			return "", reviewErrorFrom(err)
		}
		if rev.Revision != *reviewRevision {
			return "", &apiError{status: http.StatusConflict, code: "expected-revision-mismatch",
				message: "review_revision names a revision superseded by a resubmission"}
		}
		target, err := issues.RefByID(tx, rev.Issue)
		if err != nil {
			return "", issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, target.Project); apiErr != nil {
			return "", apiErr
		}
		return target.ID, nil
	}
}

type newCommentRequest struct {
	Issue          *string `json:"issue"`
	DocVersion     *string `json:"doc_version"`
	Review         *string `json:"review"`
	ReviewRevision *int64  `json:"review_revision"`
	Parent         *string `json:"parent"`
	Anchor         *string `json:"anchor"`
	Author         string  `json:"author"`
	// Body is a POINTER so an omitted property is distinguishable from
	// a present empty one: NewComment.body constrains no length and
	// AC-comment-no-cap declares no minimum, so "" is a valid body and
	// only absence is an error (review 1926).
	Body *string `json:"body"`
}

func (s *server) createComment(w http.ResponseWriter, r *http.Request) {
	// The unbounded body decodes in the PREPARE stage — before the
	// idempotency reservation takes SQLite's write lock — so a large
	// comment never stalls unrelated mutations while parsing.
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req newCommentRequest
		if apiErr := rejectExplicitNulls(r, &req, "issue", "doc_version", "review", "review_revision", "parent", "anchor", "author", "body"); apiErr != nil {
			return nil, apiErr
		}
		if req.Body == nil {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "body is required"}
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(newCommentRequest)
		if apiErr := requireActor(tx, req.Author); apiErr != nil {
			return 0, nil, apiErr
		}
		subject, apiErr := s.resolveCommentAnchor(tx, req.Issue, req.DocVersion, req.Review, req.ReviewRevision)
		if apiErr != nil {
			return 0, nil, apiErr
		}
		created, err := comments.Create(tx, comments.New{
			Issue: req.Issue, DocVersion: req.DocVersion, Review: req.Review,
			ReviewRevision: req.ReviewRevision, Parent: req.Parent, Anchor: req.Anchor,
			Author: req.Author, Body: *req.Body,
		})
		if err != nil {
			return 0, nil, commentErrorFrom(err)
		}
		payload := commentPayload(created.ID)
		if _, err := events.Emit(tx, "comment.created", subject, events.NewOperation(), req.Author, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, created, nil
	})
}

func commentPayload(commentID string) string {
	return `{"comment":"` + commentID + `"}`
}

func (s *server) listComments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var column, id string
	for _, name := range []string{"issue", "doc_version", "review"} {
		if q.Has(name) {
			if column != "" {
				writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "filter by exactly one of issue, doc_version, or review"})
				return
			}
			column, id = name, q.Get(name)
		}
	}
	if column == "" {
		writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "filter by exactly one of issue, doc_version, or review"})
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	// Comment bodies carry no cap (AC-comment-no-cap); the listing
	// streams row by row so memory holds one comment, not the thread.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{'['})
	first := true
	err = comments.EachByAnchor(tx, column, id, func(c comments.Comment) error {
		raw, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if !first {
			_, _ = w.Write([]byte{','})
		}
		first = false
		_, writeErr := w.Write(raw)
		return writeErr
	})
	if err != nil {
		return // status committed; truncation is the only signal
	}
	_, _ = w.Write([]byte{']'})
}
