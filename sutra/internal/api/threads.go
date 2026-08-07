package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
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

type importThreadRequest struct {
	Title      string          `json:"title"`
	Transcript json.RawMessage `json:"transcript"`
	Session    *string         `json:"session"`
	Project    *string         `json:"project"`
	Issue      *string         `json:"issue"`
	Actor      string          `json:"actor"`
}

func (s *server) importThread(w http.ResponseWriter, r *http.Request) {
	// The unbounded transcript decodes pre-lock (review 1871).
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req importThreadRequest
		// transcript is REQUIRED with an arbitrary-JSON value space —
		// an explicit null is a legitimate VALUE for it, unlike the
		// optional fields where null would fake absence.
		if apiErr := rejectExplicitNulls(r, &req, "title", "session", "project", "issue", "actor"); apiErr != nil {
			return nil, apiErr
		}
		if req.Title == "" {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "title is required"}
		}
		if len(req.Transcript) == 0 {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "transcript is required"}
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(importThreadRequest)
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
		body, apiErr := threadJSON(thread)
		if apiErr != nil {
			return 0, nil, apiErr
		}
		return http.StatusCreated, body, nil
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if apiErr := writeThreadJSON(w, thread); apiErr != nil {
		return // status committed; truncation is the only signal
	}
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
	streamThreadArray(w, func(fn func(threads.Thread) error) error {
		return threads.SearchEach(tx, optional("q"), optional("session"), optional("project"), fn)
	})
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
	streamThreadArray(w, func(fn func(threads.Thread) error) error {
		return threads.ListByIssueEach(tx, issueID, fn)
	})
}

func (s *server) setThreadAnchor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("threadId")
	type anchorRequest struct {
		Project *string `json:"project"`
		Issue   *string `json:"issue"`
		Actor   string  `json:"actor"`
	}
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req anchorRequest
		if apiErr := rejectExplicitNulls(r, &req, "project", "issue", "actor"); apiErr != nil {
			return nil, apiErr
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(anchorRequest)
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		current, err := threads.Get(tx, id)
		if err != nil {
			return 0, nil, threadErrorFrom(err)
		}
		// The CURRENT anchor's project guards too: a thread anchored
		// inside an archived project is part of that project's
		// read-only content and cannot be moved out.
		if apiErr := s.guardCurrentAnchor(tx, current); apiErr != nil {
			return 0, nil, apiErr
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
		body, apiErr := threadJSON(thread)
		if apiErr != nil {
			return 0, nil, apiErr
		}
		return http.StatusOK, body, nil
	})
}

// guardCurrentAnchor resolves the project governing a thread's
// existing anchor — the anchored issue's project, else the anchored
// project — and rejects if it is archived.
func (s *server) guardCurrentAnchor(tx *sql.Tx, t threads.Thread) *apiError {
	project := ""
	if t.Issue != nil {
		iss, err := issues.Get(tx, *t.Issue)
		if err != nil {
			return issueErrorFrom(err)
		}
		project = iss.Project
	} else if t.Project != nil {
		project = *t.Project
	}
	if project == "" {
		return nil
	}
	return guardWritable(tx, project)
}

// threadJSON assembles a Thread response by hand: every field but the
// transcript marshals normally, then the stored transcript bytes are
// spliced in UNTOUCHED — passing them through json.Marshal (even as a
// RawMessage or via MarshalJSON) would compact insignificant
// whitespace and break AC-thread-import's verbatim guarantee.
func threadMetaJSON(t threads.Thread) ([]byte, *apiError) {
	// Anchors FIRST: the UI's scope check reads project and issue and
	// stops there, so neither the session nor the unbounded title is
	// ever decoded on the way (review 1900).
	shadow := struct {
		ID         string  `json:"id"`
		Project    *string `json:"project,omitempty"`
		Issue      *string `json:"issue,omitempty"`
		ImportedAt string  `json:"imported_at"`
		Session    *string `json:"session"`
		Title      string  `json:"title"`
	}{t.ID, t.Project, t.Issue, t.ImportedAt, t.Session, t.Title}
	raw, err := json.Marshal(shadow)
	if err != nil {
		return nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("encode thread: %v", err)}
	}
	return raw, nil
}

// threadJSON builds one thread response as bytes. Only the MUTATION
// responses need this: the idempotency store records the response
// verbatim by contract (CON-idempotent-mutations), so those bytes must
// exist. Every read path streams via writeThreadJSON instead.
func threadJSON(t threads.Thread) (json.RawMessage, *apiError) {
	raw, apiErr := threadMetaJSON(t)
	if apiErr != nil {
		return nil, apiErr
	}
	buf := make([]byte, 0, len(raw)+len(t.Transcript)+16)
	buf = append(buf, raw[:len(raw)-1]...)
	buf = append(buf, `,"transcript":`...)
	buf = append(buf, t.Transcript...)
	buf = append(buf, '}')
	return buf, nil
}

// writeThreadJSON streams one thread response: bounded metadata, then
// the stored transcript bytes written DIRECTLY to the wire. No
// aggregate buffer sized to the transcript is ever allocated — the
// scan's own copy is the single materialization (review 1885).
func writeThreadJSON(w io.Writer, t threads.Thread) *apiError {
	raw, apiErr := threadMetaJSON(t)
	if apiErr != nil {
		return apiErr
	}
	if _, err := w.Write(raw[:len(raw)-1]); err != nil {
		return errorFrom(err)
	}
	if _, err := w.Write([]byte(`,"transcript":`)); err != nil {
		return errorFrom(err)
	}
	if _, err := w.Write(t.Transcript); err != nil {
		return errorFrom(err)
	}
	if _, err := w.Write([]byte{'}'}); err != nil {
		return errorFrom(err)
	}
	return nil
}

// streamThreadArray writes a JSON array of verbatim thread responses
// straight to the wire, one row at a time — transcripts can approach
// SQLite's value bound, so listings never accumulate the catalog in
// memory. Errors after the first byte can only truncate the stream;
// the status is already committed.
func streamThreadArray(w http.ResponseWriter, each func(fn func(threads.Thread) error) error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{'['})
	first := true
	err := each(func(t threads.Thread) error {
		if !first {
			_, _ = w.Write([]byte{','})
		}
		first = false
		if apiErr := writeThreadJSON(w, t); apiErr != nil {
			return fmt.Errorf("%s", apiErr.message)
		}
		return nil
	})
	if err != nil {
		// Mid-stream failure: the array is already partially written;
		// truncating is the only honest signal left.
		return
	}
	_, _ = w.Write([]byte{']'})
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
