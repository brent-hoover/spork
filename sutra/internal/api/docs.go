package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"sutra/internal/docs"
	"sutra/internal/events"
	"sutra/internal/issues"
	"sutra/internal/projects"
)

func docErrorFrom(err error) *apiError {
	var docNotFound *docs.NotFoundError
	var versionNotFound *docs.VersionNotFoundError
	var templateNotFound *docs.TemplateNotFoundError
	if errors.As(err, &docNotFound) || errors.As(err, &versionNotFound) || errors.As(err, &templateNotFound) {
		return &apiError{status: http.StatusNotFound, code: "not-found", message: err.Error()}
	}
	var dupName *docs.DuplicateTemplateNameError
	if errors.As(err, &dupName) {
		return &apiError{status: http.StatusConflict, code: "unique-violation", message: err.Error(), conflicts: []string{dupName.ExistingID}}
	}
	return issueErrorFrom(err)
}

// docEventSubject picks the event subject per EventBase: the tied issue
// for issue-scoped doc events, the project otherwise.
func docEventSubject(d docs.Document) string {
	if d.Issue != nil {
		return *d.Issue
	}
	return d.Project
}

type documentView struct {
	docs.Document
	Version docs.Version `json:"version"`
}

type newDocumentRequest struct {
	Title      string  `json:"title"`
	Issue      *string `json:"issue"`
	TemplateID *string `json:"template_id"`
	Content    *string `json:"content"`
	Author     string  `json:"author"`
}

func (s *server) createDocument(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("projectId")
	// Unbounded content decodes in the PREPARE stage, before the
	// write lock (review 1871).
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req newDocumentRequest
		if apiErr := rejectExplicitNulls(r, &req, "title", "issue", "template_id", "content", "author"); apiErr != nil {
			return nil, apiErr
		}
		if req.Title == "" {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "title is required"}
		}
		if (req.TemplateID == nil) == (req.Content == nil) {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "exactly one of template_id or content is required"}
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(newDocumentRequest)
		if apiErr := requireActor(tx, req.Author); apiErr != nil {
			return 0, nil, apiErr
		}
		if apiErr := guardWritable(tx, project); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Issue != nil {
			issue, err := issues.Get(tx, *req.Issue)
			if err != nil {
				return 0, nil, issueErrorFrom(err)
			}
			if issue.Project != project {
				return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "document and issue must share a project"}
			}
		}
		content := ""
		if req.Content != nil {
			content = *req.Content
		} else {
			tpl, err := docs.GetTemplate(tx, *req.TemplateID)
			if err != nil {
				return 0, nil, docErrorFrom(err)
			}
			content = tpl.Content
		}
		doc, version, err := docs.Create(tx, project, req.Title, req.Issue, content, req.Author)
		if err != nil {
			return 0, nil, docErrorFrom(err)
		}
		operation := events.NewOperation()
		payload := docPayload(doc.ID)
		if _, err := events.Emit(tx, "doc.version-saved", docEventSubject(doc), operation, req.Author, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		if doc.Issue != nil {
			if _, err := events.Emit(tx, "doc.linked", *doc.Issue, operation, req.Author, &payload); err != nil {
				return 0, nil, errorFrom(err)
			}
		}
		return http.StatusCreated, documentView{Document: doc, Version: version}, nil
	})
}

func docPayload(documentID string) string {
	raw, _ := json.Marshal(map[string]string{"document": documentID})
	return string(raw)
}

func (s *server) getDocument(w http.ResponseWriter, r *http.Request) {
	var versionNumber int64
	if raw := r.URL.Query().Get("version"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("version %q must be a positive integer", raw)})
			return
		}
		versionNumber = n
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	doc, err := docs.Get(tx, r.PathValue("documentId"))
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	version, err := docs.VersionAt(tx, doc.ID, versionNumber)
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, documentView{Document: doc, Version: version})
}

// docVersionMeta is the contract's DocVersionMeta: a DocVersion
// without its content. It exists as its own type because Version
// always marshals a content field, which the closed meta schema
// forbids.
type docVersionMeta struct {
	ID       string `json:"id"`
	Document string `json:"document"`
	Number   int64  `json:"number"`
	Author   string `json:"author"`
	Created  string `json:"created"`
}

// documentMetaView is the contract's DocumentMeta: only fixed-size
// facts. The title is deliberately absent — it is as unbounded as the
// content, and this is what a five-second poll fetches (review 1918).
type documentMetaView struct {
	ID             string         `json:"id"`
	Project        string         `json:"project"`
	Issue          *string        `json:"issue"`
	CurrentVersion *string        `json:"current_version"`
	Version        docVersionMeta `json:"version"`
}

// getDocumentMeta serves the bounded projection of getDocument: the
// document (already bounded) plus the current version WITHOUT its
// content. The web view's scope guards and its five-second
// live-refresh poll both need only these fields, and getDocument would
// read and marshal the whole unbounded content to answer either
// (reviews 1912, 1914).
func (s *server) getDocumentMeta(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	doc, err := docs.Get(tx, r.PathValue("documentId"))
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	v, err := docs.CurrentVersionMeta(tx, doc.ID)
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, documentMetaView{
		ID: doc.ID, Project: doc.Project, Issue: doc.Issue, CurrentVersion: doc.CurrentVersion,
		Version: docVersionMeta{
			ID: v.ID, Document: v.Document, Number: v.Number, Author: v.Author, Created: v.Created,
		},
	})
}

type saveVersionRequest struct {
	Content *string `json:"content"`
	Author  string  `json:"author"`
}

func (s *server) saveDocVersion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("documentId")
	// Unbounded content decodes pre-lock (review 1871).
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req saveVersionRequest
		if apiErr := rejectExplicitNulls(r, &req, "content", "author"); apiErr != nil {
			return nil, apiErr
		}
		if req.Content == nil {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "content is required"}
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(saveVersionRequest)
		if apiErr := requireActor(tx, req.Author); apiErr != nil {
			return 0, nil, apiErr
		}
		doc, err := docs.Get(tx, id)
		if err != nil {
			return 0, nil, docErrorFrom(err)
		}
		if apiErr := guardWritable(tx, doc.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		version, err := docs.SaveVersion(tx, id, *req.Content, req.Author)
		if err != nil {
			return 0, nil, docErrorFrom(err)
		}
		payload := docPayload(id)
		if _, err := events.Emit(tx, "doc.version-saved", docEventSubject(doc), events.NewOperation(), req.Author, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, version, nil
	})
}

func (s *server) listDocVersions(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	// Existence resolves before the stream commits a 200.
	if _, err := docs.Get(tx, r.PathValue("documentId")); err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	// Version contents are unbounded; the history streams one row at
	// a time instead of accumulating a slice and aggregate buffer.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{'['})
	first := true
	streamErr := docs.VersionsEach(tx, r.PathValue("documentId"), func(v docs.Version) error {
		raw, err := json.Marshal(v)
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

func (s *server) getDocVersion(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	version, err := docs.VersionByID(tx, r.PathValue("docVersionId"))
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, version)
}

func (s *server) diffDocVersions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	parse := func(name string) (int64, *apiError) {
		n, err := strconv.ParseInt(q.Get(name), 10, 64)
		if err != nil || n < 1 {
			return 0, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("%s must be a positive integer", name)}
		}
		return n, nil
	}
	from, apiErr := parse("from")
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	to, apiErr := parse("to")
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	id := r.PathValue("documentId")
	if err := docs.VersionSizesOK(tx, id, from, to); err != nil {
		var tooLarge *docs.DiffTooLargeError
		if errors.As(err, &tooLarge) {
			writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: err.Error()})
			return
		}
		writeError(w, errorFrom(err))
		return
	}
	fromVersion, err := docs.VersionAt(tx, id, from)
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	toVersion, err := docs.VersionAt(tx, id, to)
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	if err := docs.CheckDiffable(fromVersion, toVersion); err != nil {
		writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "diff": docs.UnifiedDiff(fromVersion, toVersion),
	})
}

func (s *server) linkDocumentToIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("documentId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Issue string `json:"issue"`
			Actor string `json:"actor"`
		}
		if apiErr := rejectExplicitNulls(r, &req, "issue", "actor"); apiErr != nil {
			return 0, nil, apiErr
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		doc, err := docs.Get(tx, id)
		if err != nil {
			return 0, nil, docErrorFrom(err)
		}
		if apiErr := guardWritable(tx, doc.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		issue, err := issues.Get(tx, req.Issue)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if issue.Project != doc.Project {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "document and issue must share a project"}
		}
		formerIssue := doc.Issue
		updated, err := docs.SetIssue(tx, id, &req.Issue)
		if err != nil {
			return 0, nil, docErrorFrom(err)
		}
		operation := events.NewOperation()
		payload := docPayload(id)
		// A relink is a move: the former issue's audit trail records
		// the departure under the SAME operation as the arrival — a
		// document never silently vanishes from an issue's history.
		if formerIssue != nil && *formerIssue != req.Issue {
			if _, err := events.Emit(tx, "doc.unlinked", *formerIssue, operation, req.Actor, &payload); err != nil {
				return 0, nil, errorFrom(err)
			}
		}
		if _, err := events.Emit(tx, "doc.linked", req.Issue, operation, req.Actor, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, updated, nil
	})
}

func (s *server) unlinkDocumentFromIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("documentId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		actor := r.URL.Query().Get("actor")
		if apiErr := requireActor(tx, actor); apiErr != nil {
			return 0, nil, apiErr
		}
		doc, err := docs.Get(tx, id)
		if err != nil {
			return 0, nil, docErrorFrom(err)
		}
		if apiErr := guardWritable(tx, doc.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		if doc.Issue == nil {
			return 0, nil, &apiError{status: http.StatusConflict, code: "bad-request", message: "document is not tied to an issue"}
		}
		formerIssue := *doc.Issue
		updated, err := docs.SetIssue(tx, id, nil)
		if err != nil {
			return 0, nil, docErrorFrom(err)
		}
		payload := docPayload(id)
		if _, err := events.Emit(tx, "doc.unlinked", formerIssue, events.NewOperation(), actor, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, updated, nil
	})
}

func (s *server) listProjectDocuments(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := projects.GetTx(tx, r.PathValue("projectId")); err != nil {
		writeError(w, errorFrom(err))
		return
	}
	list, err := docs.ListByProject(tx, r.PathValue("projectId"))
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) listIssueDocuments(w http.ResponseWriter, r *http.Request) {
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
	list, err := docs.ListByIssue(tx, r.PathValue("issueId"))
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ---------------------------------------------------------------- templates

// Pointers distinguish an OMITTED property from a present empty
// string. NewDocTemplate requires both properties but constrains
// neither's length, and UpdateDocTemplate already accepts empty values —
// creation must not be stricter than the contract or than its own
// update (review 1904).
type templateRequest struct {
	Name    *string `json:"name"`
	Content *string `json:"content"`
}

func (s *server) createTemplate(w http.ResponseWriter, r *http.Request) {
	// Unbounded content decodes pre-lock (review 1873).
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req templateRequest
		if apiErr := rejectExplicitNulls(r, &req, "name", "content"); apiErr != nil {
			return nil, apiErr
		}
		if req.Name == nil || req.Content == nil {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "name and content are required"}
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(templateRequest)
		created, err := docs.CreateTemplate(tx, *req.Name, *req.Content)
		if err != nil {
			return 0, nil, docErrorFrom(err)
		}
		return http.StatusCreated, created, nil
	})
}

func (s *server) listTemplates(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	// Template contents are unbounded, so the catalog streams one row
	// at a time instead of accumulating a slice and then marshaling a
	// second copy of the whole thing (review 1916). The query opens
	// BEFORE the status is committed, so a failure to start still
	// reports as an error rather than a truncated 200 (review 1918).
	cursor, err := docs.OpenTemplates(tx)
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	defer func() { _ = cursor.Close() }()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{'['})
	first := true
	streamErr := cursor.Each(func(t docs.Template) error {
		raw, err := json.Marshal(t)
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

func (s *server) getTemplate(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	tpl, err := docs.GetTemplate(tx, r.PathValue("templateId"))
	if err != nil {
		writeError(w, docErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, tpl)
}

type updateTemplateRequest struct {
	Name    *string `json:"name"`
	Content *string `json:"content"`
}

func (s *server) updateTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("templateId")
	// Unbounded content decodes pre-lock (review 1873).
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req updateTemplateRequest
		if apiErr := rejectExplicitNulls(r, &req, "name", "content"); apiErr != nil {
			return nil, apiErr
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(updateTemplateRequest)
		updated, err := docs.UpdateTemplate(tx, id, req.Name, req.Content)
		if err != nil {
			return 0, nil, docErrorFrom(err)
		}
		return http.StatusOK, updated, nil
	})
}

func (s *server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("templateId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		if err := docs.DeleteTemplate(tx, id); err != nil {
			return 0, nil, docErrorFrom(err)
		}
		return http.StatusNoContent, nil, nil
	})
}
