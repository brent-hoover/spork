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
	"net/http"

	"sutra/internal/identity"
)

// New wires the HTTP API over the given database, running each domain
// module's migrations. The composition root and the acceptance harness
// are its only callers.
func New(db *sql.DB) (http.Handler, error) {
	if err := identity.Migrate(db); err != nil {
		return nil, err
	}
	if err := migrateIdempotency(db); err != nil {
		return nil, err
	}
	s := &server{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /identities", s.createIdentity)
	mux.HandleFunc("GET /identities", s.listIdentities)
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
	var dup *identity.DuplicateHandleError
	if errors.As(err, &dup) {
		return &apiError{
			status:    http.StatusConflict,
			code:      "unique-violation",
			message:   err.Error(),
			conflicts: []string{dup.ExistingID},
		}
	}
	return &apiError{status: http.StatusInternalServerError, code: "bad-request", message: err.Error()}
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
		if apiErr := decodeBody(r, &req); apiErr != nil {
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
