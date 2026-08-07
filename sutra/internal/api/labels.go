package api

import (
	"database/sql"
	"net/http"

	"sutra/internal/events"
	"sutra/internal/issues"
)

func labelErrorFrom(err error) *apiError {
	switch e := err.(type) {
	case *issues.DuplicateLabelError:
		return &apiError{status: http.StatusConflict, code: "unique-violation", message: err.Error(), conflicts: []string{e.ExistingID}}
	case *issues.LabelNotFoundError:
		return &apiError{status: http.StatusNotFound, code: "not-found", message: err.Error()}
	case *issues.LabelAttachedError:
		// The issue_labels primary key is a uniqueness constraint; the
		// contract's closed code enum covers it as unique-violation.
		return &apiError{status: http.StatusConflict, code: "unique-violation", message: err.Error(), conflicts: []string{e.Label}}
	case *issues.LabelNotAttachedError:
		return &apiError{status: http.StatusNotFound, code: "not-found", message: err.Error()}
	default:
		return errorFrom(err)
	}
}

func (s *server) createLabel(w http.ResponseWriter, r *http.Request) {
	type createLabelRequest struct {
		Name  string  `json:"name"`
		Color *string `json:"color"`
	}
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req createLabelRequest
		if apiErr := rejectExplicitNulls(r, &req, "name", "color"); apiErr != nil {
			return nil, apiErr
		}
		if req.Name == "" {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "name is required"}
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(createLabelRequest)
		label, err := issues.CreateLabel(tx, req.Name, req.Color)
		if err != nil {
			return 0, nil, labelErrorFrom(err)
		}
		return http.StatusCreated, label, nil
	})
}

func (s *server) listLabels(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	cursor, err := issues.OpenLabels(tx)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	streamArray(w, cursor)
}

// mutateLabel runs attach/detach under the shared guards (issue
// exists, project writable, actor known), emits the event, and
// returns the refreshed IssueRead.
func (s *server) mutateLabel(tx *sql.Tx, issueID, actor, eventKind string, op func() error) (int, any, *apiError) {
	if apiErr := requireActor(tx, actor); apiErr != nil {
		return 0, nil, apiErr
	}
	issue, err := issues.Get(tx, issueID)
	if err != nil {
		return 0, nil, issueErrorFrom(err)
	}
	if apiErr := guardWritable(tx, issue.Project); apiErr != nil {
		return 0, nil, apiErr
	}
	if err := op(); err != nil {
		return 0, nil, labelErrorFrom(err)
	}
	if _, err := events.Emit(tx, eventKind, issueID, events.NewOperation(), actor, nil); err != nil {
		return 0, nil, errorFrom(err)
	}
	updated, err := issues.Get(tx, issueID)
	if err != nil {
		return 0, nil, issueErrorFrom(err)
	}
	watermark, err := events.Watermark(tx)
	if err != nil {
		return 0, nil, errorFrom(err)
	}
	return http.StatusOK, issueRead{Issue: updated, FeedWatermark: watermark}, nil
}

func (s *server) attachLabel(w http.ResponseWriter, r *http.Request) {
	issueID := r.PathValue("issueId")
	type attachRequest struct {
		Label string `json:"label"`
		Actor string `json:"actor"`
	}
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req attachRequest
		if apiErr := rejectExplicitNulls(r, &req, "label", "actor"); apiErr != nil {
			return nil, apiErr
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(attachRequest)
		return s.mutateLabel(tx, issueID, req.Actor, "issue.labeled", func() error {
			return issues.AttachLabel(tx, issueID, req.Label)
		})
	})
}

func (s *server) detachLabel(w http.ResponseWriter, r *http.Request) {
	issueID := r.PathValue("issueId")
	labelID := r.PathValue("labelId")
	actor := r.URL.Query().Get("actor")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		return s.mutateLabel(tx, issueID, actor, "issue.unlabeled", func() error {
			return issues.DetachLabel(tx, issueID, labelID)
		})
	})
}

// listIssueEvents serves the issue's audit history in chronological
// order (AC-audit-query).
func (s *server) listIssueEvents(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	issueID := r.PathValue("issueId")
	if _, err := issues.Get(tx, issueID); err != nil {
		writeError(w, issueErrorFrom(err))
		return
	}
	// Audit history streams row by row, payload bytes verbatim, like
	// the feed and the export — the history is unbounded.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{'['})
	first := true
	streamErr := events.BySubjectEach(tx, issueID, func(e events.Event) error {
		raw, err := eventJSON(e)
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
	if streamErr != nil {
		return // status committed; truncation is the only signal
	}
	_, _ = w.Write([]byte{']'})
}
