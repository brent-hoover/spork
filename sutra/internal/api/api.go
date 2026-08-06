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
	"strconv"
	"time"

	"sutra/internal/events"
	"sutra/internal/identity"
	"sutra/internal/issues"
	"sutra/internal/projects"
	"sutra/internal/review"
)

// New wires the HTTP API over the given database, running each domain
// module's migrations. The composition root and the acceptance harness
// are its only callers.
func New(db *sql.DB) (http.Handler, error) {
	for _, migrate := range []func(*sql.DB) error{identity.Migrate, projects.Migrate, events.Migrate, issues.Migrate, review.Migrate, migrateIdempotency, migratePendingPins} {
		if err := migrate(db); err != nil {
			return nil, err
		}
	}
	s := &server{db: db}
	if err := s.reconcilePins(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /identities", s.createIdentity)
	mux.HandleFunc("GET /identities", s.listIdentities)
	mux.HandleFunc("POST /projects", s.createProject)
	mux.HandleFunc("GET /projects", s.listProjects)
	mux.HandleFunc("GET /projects/{projectId}", s.getProject)
	mux.HandleFunc("POST /projects/{projectId}/archive", s.archiveProject)
	mux.HandleFunc("GET /events", s.listEvents)
	mux.HandleFunc("POST /projects/{projectId}/issues", s.createIssue)
	mux.HandleFunc("GET /projects/{projectId}/issues", s.listIssues)
	mux.HandleFunc("GET /issues/{issueId}", s.getIssue)
	mux.HandleFunc("PATCH /issues/{issueId}", s.updateIssue)
	mux.HandleFunc("POST /issues/{issueId}/status", s.updateIssueStatus)
	mux.HandleFunc("POST /issues/{issueId}/assign", s.assignIssue)
	mux.HandleFunc("POST /identities/{identityId}/work-stack/pop", s.popWorkStack)
	mux.HandleFunc("POST /issues/{issueId}/relations", s.addIssueRelation)
	mux.HandleFunc("GET /issues/{issueId}/relations", s.listIssueRelations)
	mux.HandleFunc("DELETE /issues/{issueId}/relations/{relationId}", s.removeIssueRelation)
	mux.HandleFunc("POST /reviews", s.createReview)
	mux.HandleFunc("GET /reviews", s.listReviews)
	mux.HandleFunc("GET /reviews/{reviewId}", s.getReview)
	mux.HandleFunc("POST /reviews/{reviewId}/verdict", s.setReviewVerdict)
	mux.HandleFunc("POST /reviews/{reviewId}/consume", s.consumeReviewApproval)
	mux.HandleFunc("POST /reviews/{reviewId}/resubmit", s.resubmitReview)
	mux.HandleFunc("GET /reviews/{reviewId}/deliverable", s.getReviewDeliverable)
	return mux, nil
}

type server struct {
	db *sql.DB
}

// pendingPinGrace is how long an unsettled pending pin is presumed to
// belong to an in-flight submission before reconciliation prunes it.
const pendingPinGrace = time.Hour

// reconcilePins runs at startup: every refs/sutra/pins ref that is
// neither referenced by an accepted submission nor covered by a recent
// pending row is removed, together with expired pending rows — the
// cleanup half of the write-ahead pin protocol.
func (s *server) reconcilePins() error {
	covered := map[string]bool{}
	rows, err := s.db.Query(`SELECT commit_sha, base_commit FROM review_submissions WHERE commit_sha IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("reconcile pins: read submissions: %w", err)
	}
	for rows.Next() {
		var commit, base sql.NullString
		if err := rows.Scan(&commit, &base); err != nil {
			_ = rows.Close()
			return fmt.Errorf("reconcile pins: scan: %w", err)
		}
		if commit.Valid {
			covered[commit.String] = true
		}
		if base.Valid {
			covered[base.String] = true
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reconcile pins: iterate: %w", err)
	}

	pendingRecent := map[string]bool{}
	cutoff := time.Now().UTC().Add(-pendingPinGrace).Format(time.RFC3339Nano)
	pending, err := s.db.Query(`SELECT sha, created FROM pending_pins`)
	if err != nil {
		return fmt.Errorf("reconcile pins: read pending: %w", err)
	}
	var expired []string
	for pending.Next() {
		var sha, created string
		if err := pending.Scan(&sha, &created); err != nil {
			_ = pending.Close()
			return fmt.Errorf("reconcile pins: scan pending: %w", err)
		}
		if created >= cutoff {
			pendingRecent[sha] = true
		} else {
			expired = append(expired, sha)
		}
	}
	_ = pending.Close()
	if err := pending.Err(); err != nil {
		return fmt.Errorf("reconcile pins: iterate pending: %w", err)
	}
	for _, sha := range expired {
		if _, err := s.db.Exec(`DELETE FROM pending_pins WHERE sha = ?`, sha); err != nil {
			return fmt.Errorf("reconcile pins: expire pending %s: %w", sha, err)
		}
	}

	repos, err := s.db.Query(`SELECT DISTINCT repo_path FROM projects WHERE repo_path IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("reconcile pins: read repos: %w", err)
	}
	var paths []string
	for repos.Next() {
		var p string
		if err := repos.Scan(&p); err != nil {
			_ = repos.Close()
			return fmt.Errorf("reconcile pins: scan repo: %w", err)
		}
		paths = append(paths, p)
	}
	_ = repos.Close()
	if err := repos.Err(); err != nil {
		return fmt.Errorf("reconcile pins: iterate repos: %w", err)
	}
	for _, path := range paths {
		// A missing or broken repo must not block startup — its pins
		// are unreachable anyway.
		pins, err := review.ListPins(path)
		if err != nil {
			continue
		}
		for _, sha := range pins {
			if !covered[sha] && !pendingRecent[sha] {
				_ = review.Unpin(path, sha)
			}
		}
	}
	return nil
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
	raw, err := json.Marshal(body)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"code":"bad-request","message":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
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
	return nil
}

// --------------------------------------------------------------- identities

func (s *server) createIdentity(w http.ResponseWriter, r *http.Request) {
	// Decoding and semantic validation run INSIDE the idempotent
	// wrapper: a keyed 400 is a settled outcome, and replaying the key
	// with a corrected body must return the original rejection, not
	// perform the mutation.
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Handle      string  `json:"handle"`
			Kind        string  `json:"kind"`
			DisplayName *string `json:"display_name"`
		}
		if apiErr := rejectExplicitNulls(r, &req, "handle", "kind", "display_name"); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Handle == "" || (req.Kind != "human" && req.Kind != "agent") {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "handle and kind (human|agent) are required"}
		}
		created, err := identity.Create(tx, req.Handle, req.Kind, req.DisplayName)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, created, nil
	})
}

// ---------------------------------------------------------------- projects

func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Key           string  `json:"key"`
			Name          string  `json:"name"`
			Description   *string `json:"description"`
			RepoPath      *string `json:"repo_path"`
			DefaultBranch *string `json:"default_branch"`
			Actor         string  `json:"actor"`
		}
		if apiErr := rejectExplicitNulls(r, &req, "key", "name", "description", "repo_path", "default_branch", "actor"); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Key == "" || req.Name == "" {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "key and name are required"}
		}
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
	list, err := projects.List(s.db, include)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, list)
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
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Actor string `json:"actor"`
		}
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return 0, nil, apiErr
		}
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
	page, err := events.List(s.db, q.Get("cursor"), q.Get("kind"), q.Get("subject"), limit, q.Get("until"))
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *server) listIdentities(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	list, err := identity.List(s.db, kind)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	if list == nil {
		list = []identity.Identity{}
	}
	writeJSON(w, http.StatusOK, list)
}
