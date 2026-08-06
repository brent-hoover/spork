package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"sutra/internal/identity"
)

// migrateIdempotency creates the replay table. The stored response is
// the whole idempotency contract (CON-idempotent-mutations): replaying
// a key returns the original response verbatim — success or rejection —
// and has no second effect. Keys are scoped per operation — the
// contract never requires client keys to be globally unique across
// endpoints, so the same key on two operations is two records.
func migrateIdempotency(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS idempotency_keys (
			operation TEXT NOT NULL,
			key       TEXT NOT NULL,
			status    INTEGER NOT NULL,
			body      TEXT NOT NULL,
			PRIMARY KEY (operation, key)
		)`)
	if err != nil {
		return fmt.Errorf("migrate idempotency_keys: %w", err)
	}
	return nil
}

// idempotent runs a mutating handler under the request's Idempotency-Key,
// scoped by the matched route pattern. A replayed (operation, key) pair
// returns the recorded response without invoking fn. A fresh pair runs fn
// inside one transaction; the response record commits atomically with the
// mutation, so a crash can never apply an effect whose response was not
// recorded, or vice versa. Handler rejections (4xx) are recorded and
// replayed the same way — the original response, verbatim. Concurrent
// requests with the same pair race on the primary key: the loser's
// transaction rolls back its duplicate work and the winner's committed
// response is replayed to both callers.
func (s *server) idempotent(w http.ResponseWriter, r *http.Request, fn func(tx *sql.Tx) (int, any, *apiError)) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "Idempotency-Key header is required"})
		return
	}
	operation := r.Pattern

	if s.replayed(w, operation, key) {
		return
	}

	// Buffer the body — bounded — BEFORE any transaction opens, so a
	// slow or oversized upload can never hold a database connection.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("read request body: %v", err)})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))

	status, raw, apiErr := s.attempt(operation, key, fn)
	if apiErr != nil && apiErr.status >= http.StatusInternalServerError {
		writeError(w, apiErr)
		return
	}
	if raw == nil {
		// Lost the same-key race: the winner's response is committed
		// (or about to be) — replay it.
		s.awaitReplay(w, operation, key)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// replayed writes the recorded response for (operation, key) if one
// exists, reporting whether it did.
func (s *server) replayed(w http.ResponseWriter, operation, key string) bool {
	var status int
	var body string
	err := s.db.QueryRow(`SELECT status, body FROM idempotency_keys WHERE operation = ? AND key = ?`, operation, key).Scan(&status, &body)
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
	return true
}

// attempt executes fn and records its settled response. It returns
// (0, nil, nil) when a concurrent request already settled the pair.
func (s *server) attempt(operation, key string, fn func(tx *sql.Tx) (int, any, *apiError)) (int, []byte, *apiError) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("begin: %v", err)}
	}
	defer func() { _ = tx.Rollback() }()

	status, body, apiErr := fn(tx)
	if apiErr != nil && apiErr.status >= http.StatusInternalServerError {
		// A 5xx is not a settled outcome — nothing is recorded, the
		// pair stays fresh, and a retry re-attempts the mutation.
		return 0, nil, apiErr
	}

	var raw []byte
	if apiErr != nil {
		status = apiErr.status
		raw, err = json.Marshal(errorEnvelope(apiErr))
	} else {
		raw, err = json.Marshal(body)
	}
	if err != nil {
		return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("encode response: %v", err)}
	}

	record := func(q interface {
		Exec(string, ...any) (sql.Result, error)
	}) error {
		_, err := q.Exec(`INSERT INTO idempotency_keys (operation, key, status, body) VALUES (?, ?, ?, ?)`, operation, key, status, string(raw))
		return err
	}

	if apiErr != nil {
		// Rejections settle the pair but must not keep the mutation's
		// partial work: drop the transaction, record standalone.
		_ = tx.Rollback()
		if err := record(s.db); err != nil {
			if identity.IsUniqueViolation(err) {
				return 0, nil, nil
			}
			return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("record idempotency key: %v", err)}
		}
		return status, raw, nil
	}

	if err := record(tx); err != nil {
		if identity.IsUniqueViolation(err) {
			return 0, nil, nil
		}
		return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("record idempotency key: %v", err)}
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("commit: %v", err)}
	}
	return status, raw, nil
}

// awaitReplay returns the response committed by the request that won the
// (operation, key) race. The winner records its response atomically with
// its mutation, so the row is visible at, or momentarily after, the
// loser's constraint failure.
func (s *server) awaitReplay(w http.ResponseWriter, operation, key string) {
	for range 50 {
		if s.replayed(w, operation, key) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeError(w, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: "idempotent replay unavailable"})
}

func errorEnvelope(e *apiError) map[string]any {
	body := map[string]any{"code": e.code, "message": e.message}
	if len(e.conflicts) > 0 {
		body["conflicts"] = e.conflicts
	}
	return body
}
