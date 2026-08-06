package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"sutra/internal/events"
	"sutra/internal/issues"
	"sutra/internal/projects"
)

// issueRead is the contract's IssueRead: Issue plus the REQUIRED
// feed_watermark captured atomically with the read.
type issueRead struct {
	issues.Issue
	FeedWatermark string `json:"feed_watermark"`
}

// readIssue loads an issue and captures its watermark in one
// transaction (AC-feed-watermark).
func (s *server) readIssue(id string) (issueRead, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return issueRead{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	issue, err := issues.Get(tx, id)
	if err != nil {
		return issueRead{}, err
	}
	watermark, err := events.Watermark(tx)
	if err != nil {
		return issueRead{}, err
	}
	return issueRead{Issue: issue, FeedWatermark: watermark}, nil
}

func issueErrorFrom(err error) *apiError {
	switch e := err.(type) {
	case *issues.NotFoundError, *issues.RelationNotFoundError:
		return &apiError{status: http.StatusNotFound, code: "not-found", message: err.Error()}
	case *issues.RelationExistsError:
		return &apiError{status: http.StatusConflict, code: "relation-exists", message: err.Error(), conflicts: []string{e.Existing}}
	case *issues.CycleError:
		code := "ancestry-cycle"
		if e.Kind == "blocks" {
			code = "blocking-cycle"
		}
		return &apiError{status: http.StatusConflict, code: code, message: err.Error()}
	case *issues.HasParentError:
		return &apiError{status: http.StatusConflict, code: "relation-exists", message: err.Error(), conflicts: []string{e.Parent}}
	}
	return errorFrom(err)
}

// guardWritable rejects mutations into archived projects
// (AC-project-archive: read-only).
func guardWritable(tx *sql.Tx, project string) *apiError {
	archived, err := projects.IsArchived(tx, project)
	if err != nil {
		return errorFrom(err)
	}
	if archived {
		return &apiError{status: http.StatusConflict, code: "project-archived", message: fmt.Sprintf("project %s is archived and read-only", project)}
	}
	return nil
}

func (s *server) createIssue(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("projectId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Title    string  `json:"title"`
			Body     *string `json:"body"`
			Assignee *string `json:"assignee"`
			Actor    string  `json:"actor"`
		}
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Title == "" {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "title is required"}
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		if apiErr := guardWritable(tx, project); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Assignee != nil {
			if apiErr := requireActor(tx, *req.Assignee); apiErr != nil {
				return 0, nil, apiErr
			}
		}
		issue, err := issues.Create(tx, project, req.Title, req.Body, req.Assignee)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if err := events.Emit(tx, "issue.created", issue.ID, events.NewOperation(), req.Actor, nil); err != nil {
			return 0, nil, errorFrom(err)
		}
		watermark, err := events.Watermark(tx)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, issueRead{Issue: issue, FeedWatermark: watermark}, nil
	})
}

func (s *server) getIssue(w http.ResponseWriter, r *http.Request) {
	read, err := s.readIssue(r.PathValue("issueId"))
	if err != nil {
		writeError(w, issueErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, read)
}

func (s *server) listIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := issues.Filters{
		Statuses: q["status"],
		Assignee: q.Get("assignee"),
		Query:    q.Get("q"),
	}
	if raw := q.Get("number"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("number %q is not an integer", raw)})
			return
		}
		f.Number = &n
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	list, err := issues.List(tx, r.PathValue("projectId"), f)
	if err != nil {
		writeError(w, issueErrorFrom(err))
		return
	}
	watermark, err := events.Watermark(tx)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": list, "feed_watermark": watermark})
}

func (s *server) updateIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issueId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Title *string `json:"title"`
			Body  *string `json:"body"`
			Actor string  `json:"actor"`
		}
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return 0, nil, apiErr
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		current, err := issues.Get(tx, id)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, current.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		updated, err := issues.Update(tx, id, req.Title, req.Body)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if err := events.Emit(tx, "issue.updated", id, events.NewOperation(), req.Actor, nil); err != nil {
			return 0, nil, errorFrom(err)
		}
		watermark, err := events.Watermark(tx)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, issueRead{Issue: updated, FeedWatermark: watermark}, nil
	})
}

// updateIssueStatus is the review-gated, cascade-carrying transition
// (AC-status-set, AC-parent-close-gate, AC-parent-reopen-cascade,
// AC-subtree-revision, AC-status-conditional). Complete transitions
// name a review; until the review module lands no review id can belong
// to any issue, so every complete attempt is truthfully the contract's
// 409.
func (s *server) updateIssueStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issueId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Status         string  `json:"status"`
			ExpectedStatus *string `json:"expected_status"`
			Actor          string  `json:"actor"`
			Review         *string `json:"review"`
		}
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return 0, nil, apiErr
		}
		switch req.Status {
		case issues.StatusOpen, issues.StatusInProgress, issues.StatusBlocked, issues.StatusDeferred:
			if req.Review != nil {
				return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "review is valid only on complete transitions"}
			}
		case issues.StatusComplete:
			if req.Review == nil {
				return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "complete transitions require review, review_revision, and review_verdict_event"}
			}
		default:
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("unknown status %q", req.Status)}
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		current, err := issues.Get(tx, id)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, current.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.ExpectedStatus != nil && *req.ExpectedStatus != current.Status {
			return 0, nil, &apiError{status: http.StatusConflict, code: "expected-status-mismatch",
				message: fmt.Sprintf("expected status %q, current is %q", *req.ExpectedStatus, current.Status)}
		}
		if req.Status == issues.StatusComplete {
			// No review can belong to this issue until the review
			// module exists; the named review therefore fails the
			// ownership check (AC-close-approved).
			return 0, nil, &apiError{status: http.StatusConflict, code: "missing-approval",
				message: fmt.Sprintf("review %s does not belong to issue %s", *req.Review, id)}
		}

		operation := events.NewOperation()
		affected := []string{id}
		if err := issues.SetStatus(tx, id, req.Status); err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if err := events.Emit(tx, "issue.status-changed", id, operation, req.Actor, nil); err != nil {
			return 0, nil, errorFrom(err)
		}
		// An issue entering an active status reopens every complete
		// ancestor atomically, one status event each, same operation.
		if issues.Active(req.Status) {
			reopened, err := issues.CompleteAncestors(tx, id)
			if err != nil {
				return 0, nil, errorFrom(err)
			}
			for _, ancestor := range reopened {
				if err := issues.SetStatus(tx, ancestor, issues.StatusOpen); err != nil {
					return 0, nil, issueErrorFrom(err)
				}
				if err := events.Emit(tx, "issue.status-changed", ancestor, operation, req.Actor, nil); err != nil {
					return 0, nil, errorFrom(err)
				}
			}
		}
		ancestors, err := issues.Ancestors(tx, id)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		if err := issues.BumpSubtree(tx, append(affected, ancestors...)); err != nil {
			return 0, nil, errorFrom(err)
		}
		final, err := issues.Get(tx, id)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		watermark, err := events.Watermark(tx)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, issueRead{Issue: final, FeedWatermark: watermark}, nil
	})
}

func (s *server) addIssueRelation(w http.ResponseWriter, r *http.Request) {
	from := r.PathValue("issueId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Kind  string `json:"kind"`
			To    string `json:"to"`
			Actor string `json:"actor"`
		}
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Kind != "parent_of" && req.Kind != "blocks" {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("kind %q must be parent_of or blocks", req.Kind)}
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		fromIssue, err := issues.Get(tx, from)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, fromIssue.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		rel, err := issues.AddRelation(tx, req.Kind, from, req.To)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		operation := events.NewOperation()
		if err := events.Emit(tx, "issue.relation-added", from, operation, req.Actor, nil); err != nil {
			return 0, nil, errorFrom(err)
		}
		if rel.Kind == "parent_of" {
			// Attachment bumps the new parent and its ancestors; active
			// work beneath a complete parent reopens the chain.
			affected := []string{from}
			ancestors, err := issues.Ancestors(tx, from)
			if err != nil {
				return 0, nil, errorFrom(err)
			}
			affected = append(affected, ancestors...)
			child, err := issues.Get(tx, req.To)
			if err != nil {
				return 0, nil, issueErrorFrom(err)
			}
			activeBelow := issues.Active(child.Status)
			if !activeBelow {
				if activeBelow, err = issues.ActiveInSubtree(tx, req.To); err != nil {
					return 0, nil, errorFrom(err)
				}
			}
			if activeBelow {
				for _, candidate := range append([]string{from}, ancestors...) {
					c, err := issues.Get(tx, candidate)
					if err != nil {
						return 0, nil, issueErrorFrom(err)
					}
					if c.Status == issues.StatusComplete {
						if err := issues.SetStatus(tx, candidate, issues.StatusOpen); err != nil {
							return 0, nil, issueErrorFrom(err)
						}
						if err := events.Emit(tx, "issue.status-changed", candidate, operation, req.Actor, nil); err != nil {
							return 0, nil, errorFrom(err)
						}
					}
				}
			}
			if err := issues.BumpSubtree(tx, affected); err != nil {
				return 0, nil, errorFrom(err)
			}
		}
		return http.StatusCreated, map[string]any{"relation": rel, "operation": operation}, nil
	})
}

func (s *server) listIssueRelations(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := issues.Get(tx, r.PathValue("issueId")); err != nil {
		writeError(w, issueErrorFrom(err))
		return
	}
	rels, err := issues.Relations(tx, r.PathValue("issueId"))
	if err != nil {
		writeError(w, issueErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, rels)
}

func (s *server) removeIssueRelation(w http.ResponseWriter, r *http.Request) {
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		actor := r.URL.Query().Get("actor")
		if apiErr := requireActor(tx, actor); apiErr != nil {
			return 0, nil, apiErr
		}
		rel, err := issues.RemoveRelation(tx, r.PathValue("relationId"))
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if pathIssue := r.PathValue("issueId"); rel.From != pathIssue && rel.To != pathIssue {
			return 0, nil, &apiError{status: http.StatusNotFound, code: "not-found",
				message: fmt.Sprintf("relation %s does not involve issue %s", rel.ID, pathIssue)}
		}
		payload, err := json.Marshal(map[string]string{
			"relation": rel.ID, "kind": rel.Kind, "from": rel.From, "to": rel.To,
		})
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		payloadStr := string(payload)
		if err := events.Emit(tx, "issue.relation-removed", rel.From, events.NewOperation(), actor, &payloadStr); err != nil {
			return 0, nil, errorFrom(err)
		}
		if rel.Kind == "parent_of" {
			affected := []string{rel.From}
			ancestors, err := issues.Ancestors(tx, rel.From)
			if err != nil {
				return 0, nil, errorFrom(err)
			}
			if err := issues.BumpSubtree(tx, append(affected, ancestors...)); err != nil {
				return 0, nil, errorFrom(err)
			}
		}
		return http.StatusNoContent, nil, nil
	})
}
