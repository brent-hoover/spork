package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"

	"sutra/internal/events"
	"sutra/internal/issues"
	"sutra/internal/projects"
	"sutra/internal/threads"
)

func threadErrorFrom(err error) *apiError {
	if _, ok := err.(*threads.NotFoundError); ok {
		return &apiError{status: http.StatusNotFound, code: "not-found", message: err.Error()}
	}
	return errorFrom(err)
}

// resolveAnchor validates a requested (project, issue) anchor: at
// least one side set, both sides existing, the governing project
// writable, and — when both are given — the issue belonging to that
// project. It returns the anchor plus the event subject, which is the
// anchor's aggregation root: the issue when one is set, otherwise the
// project (setThreadAnchor contract).
func resolveAnchor(tx *sql.Tx, project, issue *string) (threads.Anchor, string, *apiError) {
	if project == nil && issue == nil {
		return threads.Anchor{}, "", &apiError{status: http.StatusBadRequest, code: "bad-request", message: "at least one of project or issue is required"}
	}
	subject := ""
	if issue != nil {
		iss, err := issues.Get(tx, *issue)
		if err != nil {
			return threads.Anchor{}, "", issueErrorFrom(err)
		}
		if project != nil && *project != iss.Project {
			return threads.Anchor{}, "", &apiError{status: http.StatusBadRequest, code: "bad-request", message: "issue does not belong to the given project"}
		}
		if apiErr := guardWritable(tx, iss.Project); apiErr != nil {
			return threads.Anchor{}, "", apiErr
		}
		subject = *issue
	} else {
		if _, err := projects.GetTx(tx, *project); err != nil {
			return threads.Anchor{}, "", errorFrom(err)
		}
		if apiErr := guardWritable(tx, *project); apiErr != nil {
			return threads.Anchor{}, "", apiErr
		}
		subject = *project
	}
	return threads.Anchor{Project: project, Issue: issue}, subject, nil
}

func (s *server) importThread(w http.ResponseWriter, r *http.Request) {
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Title      string          `json:"title"`
			Transcript json.RawMessage `json:"transcript"`
			Session    *string         `json:"session"`
			Project    *string         `json:"project"`
			Issue      *string         `json:"issue"`
			Actor      string          `json:"actor"`
		}
		if apiErr := rejectExplicitNulls(r, &req, "title", "transcript", "session", "project", "issue", "actor"); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Title == "" {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "title is required"}
		}
		if len(req.Transcript) == 0 {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "transcript is required"}
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		anchor, subject, apiErr := resolveAnchor(tx, req.Project, req.Issue)
		if apiErr != nil {
			return 0, nil, apiErr
		}
		thread, err := threads.Create(tx, req.Title, req.Transcript, req.Session, anchor)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		payload := threadPayload(thread.ID)
		if _, err := events.Emit(tx, "thread.imported", subject, events.NewOperation(), req.Actor, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, thread, nil
	})
}

func (s *server) getThread(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	thread, err := threads.Get(tx, r.PathValue("threadId"))
	if err != nil {
		writeError(w, threadErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

func (s *server) searchThreads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	optional := func(name string) *string {
		if !q.Has(name) {
			return nil
		}
		v := q.Get(name)
		return &v
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	found, err := threads.Search(tx, optional("q"), optional("session"), optional("project"))
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, found)
}

func (s *server) listIssueThreads(w http.ResponseWriter, r *http.Request) {
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
	found, err := threads.ListByIssue(tx, issueID)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, found)
}

func (s *server) setThreadAnchor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("threadId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Project *string `json:"project"`
			Issue   *string `json:"issue"`
			Actor   string  `json:"actor"`
		}
		if apiErr := rejectExplicitNulls(r, &req, "project", "issue", "actor"); apiErr != nil {
			return 0, nil, apiErr
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		if _, err := threads.Get(tx, id); err != nil {
			return 0, nil, threadErrorFrom(err)
		}
		anchor, subject, apiErr := resolveAnchor(tx, req.Project, req.Issue)
		if apiErr != nil {
			return 0, nil, apiErr
		}
		thread, old, err := threads.SetAnchor(tx, id, anchor)
		if err != nil {
			return 0, nil, threadErrorFrom(err)
		}
		payload, err := anchorChangePayload(thread.ID, old, anchor)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		if _, err := events.Emit(tx, "thread.anchor-changed", subject, events.NewOperation(), req.Actor, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, thread, nil
	})
}

func threadPayload(threadID string) string {
	raw, _ := json.Marshal(map[string]string{"thread": threadID})
	return string(raw)
}

// anchorChangePayload carries both the old and new anchors, so the
// audit trail records the departure and the arrival in one event
// (AC-thread-anchor's retarget history).
func anchorChangePayload(threadID string, old, new threads.Anchor) (string, error) {
	raw, err := json.Marshal(map[string]any{"thread": threadID, "old": old, "new": new})
	if err != nil {
		return "", fmt.Errorf("marshal anchor payload: %w", err)
	}
	return string(raw), nil
}
