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
	// slow or oversized upload can never hold a database connection. A
	// read failure (oversize included) is a settled 400: it records
	// under the pair and replays like any other keyed rejection.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		s.settleRejection(w, operation, key, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("read request body: %v", err)})
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
	writeRecorded(w, status, raw)
}

// writeRecorded writes a settled response; a 204 carries no body.
func writeRecorded(w http.ResponseWriter, status int, raw []byte) {
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// settleRejection records a pre-handler 400 under the pair — first
// writer wins, and a lost race replays the winner's response.
func (s *server) settleRejection(w http.ResponseWriter, operation, key string, apiErr *apiError) {
	raw, err := json.Marshal(errorEnvelope(apiErr))
	if err != nil {
		writeError(w, apiErr)
		return
	}
	if err := s.recordSettled(operation, key, apiErr.status, raw); err != nil {
		if identity.IsUniqueViolation(err) {
			s.awaitReplay(w, operation, key)
			return
		}
		// The key is NOT settled; returning the 400 would let a retry
		// mutate. An unsettled 500 keeps the pair fresh, like attempt.
		writeError(w, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("record idempotency key: %v", err)})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(apiErr.status)
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
	writeRecorded(w, status, []byte(body))
	return true
}

// attempt executes fn and records its settled response. It returns
// (0, nil, nil) when a concurrent request already settled the pair.
// The pair is RESERVED as the transaction's first statement: the insert
// takes SQLite's write lock immediately — no deferred-read lock to
// upgrade, so concurrent mutations serialize instead of failing BUSY —
// and a same-key race is detected before any work runs. The reservation
// commits only with the mutation; a crash leaves nothing behind.
func (s *server) attempt(operation, key string, fn func(tx *sql.Tx) (int, any, *apiError)) (int, []byte, *apiError) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("begin: %v", err)}
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`INSERT INTO idempotency_keys (operation, key, status, body) VALUES (?, ?, 0, '')`, operation, key); err != nil {
		if identity.IsUniqueViolation(err) {
			return 0, nil, nil
		}
		return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("reserve idempotency key: %v", err)}
	}

	// Handler work runs inside a savepoint: a 4xx rolls back only the
	// mutation's partial work while the reservation stays held, so no
	// concurrent same-key request can slip in a different outcome
	// between a rejection and its record.
	if _, err := tx.Exec(`SAVEPOINT handler`); err != nil {
		return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("savepoint: %v", err)}
	}

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
	} else if body != nil {
		raw, err = json.Marshal(body)
	} else {
		raw = []byte{}
	}
	if err != nil {
		return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("encode response: %v", err)}
	}

	if apiErr != nil {
		// Rejections settle the pair but must not keep the mutation's
		// partial work: unwind to the savepoint — the reservation
		// survives — then record the 4xx and commit atomically.
		if _, err := tx.Exec(`ROLLBACK TO handler`); err != nil {
			return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("rollback to savepoint: %v", err)}
		}
	}

	if _, err := tx.Exec(`UPDATE idempotency_keys SET status = ?, body = ? WHERE operation = ? AND key = ?`,
		status, string(raw), operation, key); err != nil {
		return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("record idempotency key: %v", err)}
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("commit: %v", err)}
	}
	return status, raw, nil
}

func (s *server) recordSettled(operation, key string, status int, raw []byte) error {
	_, err := s.db.Exec(`INSERT INTO idempotency_keys (operation, key, status, body) VALUES (?, ?, ?, ?)`,
		operation, key, status, string(raw))
	return err
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
