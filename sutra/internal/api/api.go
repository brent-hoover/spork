// Package api — see MOD-api in avspec.yaml. Composes the domain modules
// into the HTTP surface pinned by contracts/sutra.openapi.yaml: it owns
// routing, the error envelope, transactions, and idempotent replay;
// domain packages own their tables and rules.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"sutra/internal/comments"
	"sutra/internal/docs"
	"sutra/internal/events"
	"sutra/internal/identity"
	"sutra/internal/issues"
	"sutra/internal/projects"
	"sutra/internal/review"
	"sutra/internal/threads"
)

// New wires the HTTP API over the given database, running each domain
// module's migrations. The composition root and the acceptance harness
// are its only callers.
func New(db *sql.DB) (http.Handler, error) {
	for _, migrate := range []func(*sql.DB) error{identity.Migrate, projects.Migrate, events.Migrate, issues.Migrate, review.Migrate, docs.Migrate, threads.Migrate, comments.Migrate, migrateIdempotency} {
		if err := migrate(db); err != nil {
			return nil, err
		}
	}
	s := &server{db: db}
	mux := http.NewServeMux()
	// Every route goes on through guardIdentifiers, so no handler can be
	// added that reads an id from its path without the form check:
	// registering it IS the check (AC-identity-canonical-casing). The
	// trailing names are the route's own uuid query parameters, as the
	// contract declares them.
	handle := func(pattern string, h http.HandlerFunc, uuidQueries ...string) {
		mux.HandleFunc(pattern, guardIdentifiers(pattern, uuidQueries, h))
	}
	handle("POST /identities", s.createIdentity)
	handle("GET /identities", s.listIdentities)
	handle("POST /projects", s.createProject)
	handle("GET /projects", s.listProjects)
	handle("GET /projects/{projectId}", s.getProject)
	handle("POST /projects/{projectId}/archive", s.archiveProject)
	handle("GET /events", s.listEvents)
	handle("POST /projects/{projectId}/issues", s.createIssue)
	handle("GET /projects/{projectId}/issues", s.listIssues, "assignee")
	handle("GET /issues/{issueId}", s.getIssue)
	handle("PATCH /issues/{issueId}", s.updateIssue)
	handle("POST /issues/{issueId}/status", s.updateIssueStatus)
	handle("POST /issues/{issueId}/assign", s.assignIssue)
	handle("POST /identities/{identityId}/work-stack/pop", s.popWorkStack)
	handle("POST /issues/{issueId}/relations", s.addIssueRelation)
	handle("GET /issues/{issueId}/relations", s.listIssueRelations)
	handle("DELETE /issues/{issueId}/relations/{relationId}", s.removeIssueRelation, "actor")
	handle("POST /reviews", s.createReview)
	handle("GET /reviews", s.listReviews, "issue")
	handle("GET /reviews/{reviewId}", s.getReview)
	handle("POST /reviews/{reviewId}/verdict", s.setReviewVerdict)
	handle("POST /reviews/{reviewId}/consume", s.consumeReviewApproval)
	handle("POST /reviews/{reviewId}/resubmit", s.resubmitReview)
	handle("GET /reviews/{reviewId}/deliverable", s.getReviewDeliverable)
	handle("POST /projects/{projectId}/documents", s.createDocument)
	handle("GET /projects/{projectId}/documents", s.listProjectDocuments)
	handle("GET /issues/{issueId}/documents", s.listIssueDocuments)
	handle("GET /issues/{issueId}/events", s.listIssueEvents)
	handle("POST /labels", s.createLabel)
	handle("POST /comments", s.createComment)
	handle("GET /comments", s.listComments, "issue", "doc_version", "review")
	handle("GET /labels", s.listLabels)
	handle("POST /issues/{issueId}/labels", s.attachLabel)
	handle("DELETE /issues/{issueId}/labels/{labelId}", s.detachLabel, "actor")
	handle("POST /threads", s.importThread)
	handle("GET /threads/search", s.searchThreads, "project")
	handle("GET /search", s.search, "project")
	handle("GET /projects/{projectId}/export", s.exportProject)
	handle("POST /projects/import", s.importProject, "actor")
	handle("GET /threads/{threadId}", s.getThread)
	handle("POST /threads/{threadId}/anchor", s.setThreadAnchor)
	handle("GET /issues/{issueId}/threads", s.listIssueThreads)
	handle("GET /documents/{documentId}", s.getDocument)
	handle("POST /documents/{documentId}/versions", s.saveDocVersion)
	handle("GET /documents/{documentId}/meta", s.getDocumentMeta)
	handle("GET /documents/{documentId}/versions", s.listDocVersions)
	handle("GET /doc-versions/{docVersionId}", s.getDocVersion)
	handle("GET /documents/{documentId}/diff", s.diffDocVersions)
	handle("POST /documents/{documentId}/issue", s.linkDocumentToIssue)
	handle("DELETE /documents/{documentId}/issue", s.unlinkDocumentFromIssue, "actor")
	handle("POST /templates", s.createTemplate)
	handle("GET /templates", s.listTemplates)
	handle("GET /templates/{templateId}", s.getTemplate)
	handle("PUT /templates/{templateId}", s.updateTemplate)
	handle("DELETE /templates/{templateId}", s.deleteTemplate)
	return mux, nil
}

type server struct {
	db *sql.DB
}

// apiError is a handler-produced rejection carrying the contract's
// Error envelope fields (code from the closed enum, conflicts required
// for unique-violation).
type apiError struct {
	status    int
	code      string
	message   string
	conflicts []string // colliding resource ids (Error.conflicts)
}

func (e *apiError) Error() string { return e.message }

func errorFrom(err error) *apiError {
	var dupHandle *identity.DuplicateHandleError
	if errors.As(err, &dupHandle) {
		return &apiError{status: http.StatusConflict, code: "unique-violation", message: err.Error(), conflicts: []string{dupHandle.ExistingID}}
	}
	var dupKey *projects.DuplicateKeyError
	if errors.As(err, &dupKey) {
		return &apiError{status: http.StatusConflict, code: "unique-violation", message: err.Error(), conflicts: []string{dupKey.ExistingID}}
	}
	var archived *projects.AlreadyArchivedError
	if errors.As(err, &archived) {
		return &apiError{status: http.StatusConflict, code: "project-archived", message: err.Error()}
	}
	var notFound *projects.NotFoundError
	if errors.As(err, &notFound) {
		return &apiError{status: http.StatusNotFound, code: "not-found", message: err.Error()}
	}
	var badCursor *events.BadCursorError
	if errors.As(err, &badCursor) {
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: err.Error()}
	}
	return &apiError{status: http.StatusInternalServerError, code: "bad-request", message: err.Error()}
}

// requireActor enforces AC-identity-referenced inside the mutating
// transaction: actor is an identity id, and an unknown id is rejected,
// never silently created.
func requireActor(tx *sql.Tx, actor string) *apiError {
	if actor == "" {
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: "actor is required"}
	}
	ok, err := identity.Exists(tx, actor)
	if err != nil {
		return &apiError{status: http.StatusInternalServerError, code: "bad-request", message: err.Error()}
	}
	if !ok {
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("actor %q names no identity", actor)}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	// Pre-assembled bodies (thread responses carrying verbatim
	// transcripts) pass through untouched — json.Marshal would compact
	// embedded RawMessage bytes.
	if rm, ok := body.(json.RawMessage); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(rm)
		return
	}
	raw, err := json.Marshal(body)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"code":"bad-request","message":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// rowCursor is a query that has ALREADY opened and is ready to deliver
// rows one at a time.
type rowCursor[T any] interface {
	Each(fn func(T) error) error
	Close() error
}

// streamArray serves a JSON array from an opened cursor. Every listing
// uses it for two reasons: records marshal and write one at a time, so
// an unconstrained field (a title, a display name, a label color) is
// resident once rather than for the whole catalog (review 1922); and
// the caller opens the query BEFORE calling, so a query that fails to
// start is still an error status rather than a truncated 200
// (review 1918).
func streamArray[T any](w http.ResponseWriter, c rowCursor[T]) {
	defer func() { _ = c.Close() }()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{'['})
	first := true
	if err := c.Each(func(v T) error {
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
	}); err != nil {
		return // status committed; truncation is the only signal
	}
	_, _ = w.Write([]byte{']'})
}

func writeError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.status, errorEnvelope(e))
}

func decodeBody(r *http.Request, into any) *apiError {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(into); err != nil {
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("malformed request body: %v", err)}
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: "malformed request body: trailing data after JSON value"}
	}
	return guardIdentifierFields(into)
}

// identifierForm says what a refusal should have received, so an
// identity id is not described as an anonymous uuid.
func identifierForm(name string) string {
	if name == "actor" || name == "author" || name == "assignee" {
		return "an identity uuid"
	}
	return "a uuid"
}

// guardIdentifiers wraps one route so the identifiers in its PATH and the
// uuid query parameters IT DECLARES are refused for their form before the
// handler runs. Path parameters are read out of the registered pattern,
// so a new route is covered by being registered; every path parameter in
// this contract is an entity id. Query parameters are per-route on
// purpose: the same word is an entity id on one operation and a plain
// filter on another — `label` names a label id in a body and a label NAME
// in the issue listing — so a global list would refuse requests the
// contract permits.
func guardIdentifiers(pattern string, uuidQueries []string, h http.HandlerFunc) http.HandlerFunc {
	var params []string
	for rest := pattern; ; {
		_, after, found := strings.Cut(rest, "{")
		if !found {
			break
		}
		name, tail, closed := strings.Cut(after, "}")
		if !closed {
			break
		}
		params = append(params, name)
		rest = tail
	}
	return func(w http.ResponseWriter, r *http.Request) {
		for _, name := range params {
			if v := r.PathValue(name); !isUUID(v) {
				writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request",
					message: fmt.Sprintf("%s must be a uuid", name)})
				return
			}
		}
		query := r.URL.Query()
		for _, name := range uuidQueries {
			// Has, not Get: `?issue=` SUPPLIED an identifier and it is
			// not one. Reading the empty string as absence would widen
			// the listing to everything, which is an answer rather than
			// a refusal.
			if !query.Has(name) {
				continue
			}
			if v := query.Get(name); !isUUID(v) {
				writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request",
					message: fmt.Sprintf("%s must be %s", name, identifierForm(name))})
				return
			}
		}
		h(w, r)
	}
}

// identifierFields names every request field the contract declares as a
// UUID, mapped to how a refusal says what it should have been. It is the
// body half of AC-identity-canonical-casing: the same rule holds at the
// path and query doors (guardIdentifiers), and the three of them are the
// whole set of ways an identifier enters.
//
// `session` is deliberately absent — a session id is an agent's own
// opaque string, not an entity key.
var identifierFields = map[string]bool{
	"actor":                  true,
	"author":                 true,
	"assignee":               true,
	"issue":                  true,
	"project":                true,
	"review":                 true,
	"thread":                 true,
	"document":               true,
	"doc_version":            true,
	"parent":                 true,
	"to":                     true,
	"label":                  true, // a label ID in a body; the issue listing's `label` query is a NAME
	"template_id":            true,
	"expected_verdict_event": true,
	"review_verdict_event":   true,
}

// guardIdentifierFields refuses a decoded body carrying an identifier in
// any form but the canonical one, BEFORE the value reaches a store —
// where a miss would come back as an id that names nothing, which is a
// different claim and a false one. Absent fields are not this check's
// business: requiredness belongs to the handler that knows it.
func guardIdentifierFields(into any) *apiError {
	v := reflect.ValueOf(into)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		// A body decoded into a map carries no field names to walk.
		// Those handlers convert to their typed request and call this
		// again on it — see assignIssue.
		return nil
	}
	return guardIdentifierStruct(v)
}

func guardIdentifierStruct(v reflect.Value) *apiError {
	t := v.Type()
	for i := range t.NumField() {
		f := v.Field(i)
		// An embedded struct's fields are the OUTER body's fields to
		// every client: `doc_version` sits inside deliverableFields and
		// arrives at the top level of the JSON, so skipping it here
		// would leave the review doors unguarded.
		if t.Field(i).Anonymous && f.Kind() == reflect.Struct {
			if apiErr := guardIdentifierStruct(f); apiErr != nil {
				return apiErr
			}
			continue
		}
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if !identifierFields[name] {
			continue
		}
		if f.Kind() == reflect.Pointer {
			if f.IsNil() {
				continue
			}
			f = f.Elem()
		}
		if f.Kind() != reflect.String || f.String() == "" {
			continue
		}
		if !isUUID(f.String()) {
			return &apiError{status: http.StatusBadRequest, code: "bad-request",
				message: fmt.Sprintf("%s must be %s", name, identifierForm(name))}
		}
	}
	return nil
}

// --------------------------------------------------------------- identities

func (s *server) createIdentity(w http.ResponseWriter, r *http.Request) {
	// Decoding and body-only validation run in the PREPARE stage —
	// before the reservation takes SQLite's write lock — so an
	// unbounded display_name is decoded and rejected without any other
	// mutation waiting behind it (review 1920). A prepare rejection is
	// still a settled outcome: replaying the key with a corrected body
	// returns the original rejection rather than mutating.
	type createIdentityRequest struct {
		Handle      string  `json:"handle"`
		Kind        string  `json:"kind"`
		DisplayName *string `json:"display_name"`
	}
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req createIdentityRequest
		if apiErr := rejectExplicitNulls(r, &req, "handle", "kind", "display_name"); apiErr != nil {
			return nil, apiErr
		}
		if req.Handle == "" || (req.Kind != "human" && req.Kind != "agent") {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "handle and kind (human|agent) are required"}
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(createIdentityRequest)
		created, err := identity.Create(tx, req.Handle, req.Kind, req.DisplayName)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, created, nil
	})
}

// ---------------------------------------------------------------- projects

func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	type createProjectRequest struct {
		Key           string  `json:"key"`
		Name          string  `json:"name"`
		Description   *string `json:"description"`
		RepoPath      *string `json:"repo_path"`
		DefaultBranch *string `json:"default_branch"`
		Actor         string  `json:"actor"`
	}
	// Body decode and body-only checks run pre-lock (review 1920);
	// actor existence needs the transaction and stays below.
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req createProjectRequest
		if apiErr := rejectExplicitNulls(r, &req, "key", "name", "description", "repo_path", "default_branch", "actor"); apiErr != nil {
			return nil, apiErr
		}
		if req.Key == "." || req.Key == ".." {
			// Dot-segment keys make the declared /p/:key web routes
			// unreachable — URL canonicalization swallows them.
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "key must not be a dot segment"}
		}
		if req.Key == "" || req.Name == "" {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "key and name are required"}
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(createProjectRequest)
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		created, err := projects.Create(tx, projects.New{
			Key: req.Key, Name: req.Name, Description: req.Description,
			RepoPath: req.RepoPath, DefaultBranch: req.DefaultBranch,
		})
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		if _, err := events.Emit(tx, "project.created", created.ID, events.NewOperation(), req.Actor, nil); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, created, nil
	})
}

func (s *server) listProjects(w http.ResponseWriter, r *http.Request) {
	include := r.URL.Query().Get("includeArchived") == "true"
	cursor, err := projects.OpenList(s.db, include)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	streamArray(w, cursor)
}

func (s *server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := projects.Get(s.db, r.PathValue("projectId"))
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) archiveProject(w http.ResponseWriter, r *http.Request) {
	type archiveRequest struct {
		Actor string `json:"actor"`
	}
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req archiveRequest
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return nil, apiErr
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(archiveRequest)
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		archived, err := projects.Archive(tx, r.PathValue("projectId"))
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		if _, err := events.Emit(tx, "project.archived", archived.ID, events.NewOperation(), req.Actor, nil); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, archived, nil
	})
}

// ------------------------------------------------------------------ events

func (s *server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("limit %q out of range [1,1000]", raw)})
			return
		}
		limit = n
	}
	// Bad-cursor rejections must precede the committed 200, so bounds
	// validate on a zero-limit probe first; then rows stream — event
	// payloads serve their STORED bytes verbatim, never accumulated.
	if _, err := events.List(s.db, q.Get("cursor"), q.Get("kind"), q.Get("subject"), 0, q.Get("until")); err != nil {
		writeError(w, errorFrom(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"events":[`))
	first := true
	page, err := events.ListStream(s.db, q.Get("cursor"), q.Get("kind"), q.Get("subject"), limit, q.Get("until"), func(e events.Event) error {
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
	if err != nil {
		return // status committed; truncation is the only signal
	}
	_, _ = fmt.Fprintf(w, `],"next_cursor":%q,"drained":%t}`, page.NextCursor, page.Drained)
}

// eventJSON marshals an event with its payload bytes spliced in
// UNTOUCHED, the same treatment export gives them.
func eventJSON(e events.Event) ([]byte, error) {
	payload := e.Payload
	e.Payload = nil
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(payload) > 0 {
		raw = append(raw[:len(raw)-1], []byte(`,"payload":`)...)
		raw = append(raw, payload...)
		raw = append(raw, '}')
	}
	return raw, nil
}

func (s *server) listIdentities(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	cursor, err := identity.OpenList(s.db, kind)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	streamArray(w, cursor)
}
