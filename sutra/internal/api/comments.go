package api

import (
	"database/sql"
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
		target, err := issues.Get(tx, *issue)
		if err != nil {
			return "", issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, target.Project); apiErr != nil {
			return "", apiErr
		}
		return target.ID, nil
	case docVersion != nil:
		version, err := docs.VersionByID(tx, *docVersion)
		if err != nil {
			return "", docErrorFrom(err)
		}
		doc, err := docs.Get(tx, version.Document)
		if err != nil {
			return "", docErrorFrom(err)
		}
		if apiErr := guardWritable(tx, doc.Project); apiErr != nil {
			return "", apiErr
		}
		return docEventSubject(doc), nil
	default:
		rev, err := review.Get(tx, *reviewID)
		if err != nil {
			return "", reviewErrorFrom(err)
		}
		if rev.Revision != *reviewRevision {
			return "", &apiError{status: http.StatusConflict, code: "stale-revision",
				message: "review_revision names a revision superseded by a resubmission"}
		}
		target, err := issues.Get(tx, rev.Issue)
		if err != nil {
			return "", issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, target.Project); apiErr != nil {
			return "", apiErr
		}
		return target.ID, nil
	}
}

func (s *server) createComment(w http.ResponseWriter, r *http.Request) {
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Issue          *string `json:"issue"`
			DocVersion     *string `json:"doc_version"`
			Review         *string `json:"review"`
			ReviewRevision *int64  `json:"review_revision"`
			Parent         *string `json:"parent"`
			Anchor         *string `json:"anchor"`
			Author         string  `json:"author"`
			Body           string  `json:"body"`
		}
		if apiErr := rejectExplicitNulls(r, &req, "issue", "doc_version", "review", "review_revision", "parent", "anchor", "author", "body"); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Body == "" {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "body is required"}
		}
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
			Author: req.Author, Body: req.Body,
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
	list, err := comments.ListByAnchor(tx, column, id)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, list)
}
