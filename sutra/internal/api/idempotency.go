package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
)

// migrateIdempotency creates the replay table. The stored response is
// the whole idempotency contract (CON-idempotent-mutations): replaying
// a key returns the original response verbatim — success or rejection —
// and has no second effect.
func migrateIdempotency(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS idempotency_keys (
			key    TEXT PRIMARY KEY,
			status INTEGER NOT NULL,
			body   TEXT NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("migrate idempotency_keys: %w", err)
	}
	return nil
}

// idempotent runs a mutating handler under the request's Idempotency-Key.
// A replayed key returns the recorded response without invoking fn. A
// fresh key runs fn inside one transaction; the response record commits
// atomically with the mutation, so a crash can never apply an effect
// whose response was not recorded, or vice versa. Handler rejections
// (4xx) are recorded and replayed the same way — the original response,
// verbatim.
func (s *server) idempotent(w http.ResponseWriter, r *http.Request, fn func(tx *sql.Tx) (int, any, *apiError)) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "Idempotency-Key header is required"})
		return
	}

	var status int
	var body string
	err := s.db.QueryRow(`SELECT status, body FROM idempotency_keys WHERE key = ?`, key).Scan(&status, &body)
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	case err != sql.ErrNoRows:
		writeError(w, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("idempotency lookup: %v", err)})
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("begin: %v", err)})
		return
	}
	defer func() { _ = tx.Rollback() }()

	respStatus, respBody, apiErr := fn(tx)
	var raw []byte
	if apiErr != nil {
		respStatus = apiErr.status
		envelope := map[string]any{"code": apiErr.code, "message": apiErr.message}
		if len(apiErr.conflicts) > 0 {
			envelope["conflicts"] = apiErr.conflicts
		}
		raw, err = json.Marshal(envelope)
	} else {
		raw, err = json.Marshal(respBody)
	}
	if err != nil {
		writeError(w, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("encode response: %v", err)})
		return
	}

	// A 5xx is not a settled outcome — nothing is recorded, the key
	// stays fresh, and a retry re-attempts the mutation.
	if respStatus >= http.StatusInternalServerError {
		writeError(w, apiErr)
		return
	}

	if _, err := tx.Exec(`INSERT INTO idempotency_keys (key, status, body) VALUES (?, ?, ?)`, key, respStatus, string(raw)); err != nil {
		writeError(w, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("record idempotency key: %v", err)})
		return
	}
	if apiErr != nil {
		// Rejections settle the key but must not keep the mutation's
		// partial work: commit a transaction containing only the key
		// record by rolling back first, then recording standalone.
		_ = tx.Rollback()
		if _, err := s.db.Exec(`INSERT INTO idempotency_keys (key, status, body) VALUES (?, ?, ?)`, key, respStatus, string(raw)); err != nil {
			writeError(w, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("record idempotency key: %v", err)})
			return
		}
	} else if err := tx.Commit(); err != nil {
		writeError(w, &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("commit: %v", err)})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(respStatus)
	_, _ = w.Write(raw)
}
