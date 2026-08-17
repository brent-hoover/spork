package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRejectionSettlesOnlyBelow5xx straddles the line between a
// rejection the client caused and a failure the server had, at BOTH
// stages that can raise one — prepare, before any transaction, and the
// handler, inside it. Both write the same status to the same response,
// so the caller cannot tell them apart; the difference is whether the
// (operation, key) pair is spent. Settle a 500 and a transient failure
// becomes permanent — every retry replays the error instead of running.
// Leave a 4xx unsettled and the rejection is re-derived on replay rather
// than returned verbatim, which is the whole idempotency contract.
func TestRejectionSettlesOnlyBelow5xx(t *testing.T) {
	db := migratedDB(t)
	s := &server{db: db}

	statuses := []struct {
		name    string
		status  int
		settled bool
	}{
		{name: "the last client error", status: 499, settled: true},
		{name: "the first server error", status: http.StatusInternalServerError},
	}
	// Each stage rejects with the given status; nothing else differs.
	stages := []struct {
		name string
		run  func(*server, http.ResponseWriter, *http.Request, *apiError)
	}{
		{name: "prepare", run: func(s *server, w http.ResponseWriter, r *http.Request, rejection *apiError) {
			s.idempotentPrepared(w, r,
				func(*http.Request) (any, *apiError) { return nil, rejection },
				func(*sql.Tx, any) (int, any, *apiError) {
					t.Error("the handler ran after prepare rejected")
					return http.StatusOK, struct{}{}, nil
				})
		}},
		{name: "handler", run: func(s *server, w http.ResponseWriter, r *http.Request, rejection *apiError) {
			s.idempotent(w, r, func(*sql.Tx) (int, any, *apiError) { return 0, nil, rejection })
		}},
	}

	for _, stage := range stages {
		for i, tc := range statuses {
			t.Run(stage.name+" returns "+tc.name, func(t *testing.T) {
				key := fmt.Sprintf("%s-%d", stage.name, i)
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
				req.Header.Set("Idempotency-Key", key)

				rec := httptest.NewRecorder()
				stage.run(s, rec, req,
					&apiError{status: tc.status, code: "bad-request", message: "rejected"})

				// An unsettled failure has no record to replay, so the
				// rejection itself is the only thing that can be served.
				if rec.Code != tc.status {
					t.Fatalf("responded %d to a %d rejection: %s", rec.Code, tc.status, rec.Body)
				}
				if !strings.Contains(rec.Body.String(), "rejected") {
					t.Fatalf("the rejection's own message was not served: %s", rec.Body)
				}

				var recorded int
				if err := db.QueryRow(
					`SELECT count(*) FROM idempotency_keys WHERE operation = ? AND key = ?`,
					req.Pattern, key).Scan(&recorded); err != nil {
					t.Fatalf("count records: %v", err)
				}
				switch {
				case tc.settled && recorded == 0:
					t.Fatal("a client rejection left the key fresh; a retry would run the mutation twice")
				case !tc.settled && recorded != 0:
					t.Fatal("a server failure spent the key; every retry would replay the error")
				}
			})
		}
	}
}
