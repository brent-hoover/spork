package api_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"sutra/internal/api"
)

func startAPI(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=busy_timeout(5000)", t.Name()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	handler, err := api.New(db)
	if err != nil {
		t.Fatalf("wire api: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(func() { srv.Close(); _ = db.Close() })
	return srv, db
}

func post(t *testing.T, srv *httptest.Server, path, key, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, string(raw)
}

func TestReplayReturnsOriginalResponse(t *testing.T) {
	srv, db := startAPI(t)
	status1, body1 := post(t, srv, "/identities", "k1", `{"handle":"a","kind":"agent"}`)
	if status1 != http.StatusCreated {
		t.Fatalf("create: %d %s", status1, body1)
	}
	status2, body2 := post(t, srv, "/identities", "k1", `{"handle":"totally-different","kind":"human"}`)
	if status2 != status1 || body2 != body1 {
		t.Fatalf("replay differs: %d %s vs %d %s", status2, body2, status1, body1)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identities`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("replay had a second effect: %d identities", count)
	}
}

func TestRejectionsReplayVerbatim(t *testing.T) {
	srv, _ := startAPI(t)
	if status, body := post(t, srv, "/identities", "k1", `{"handle":"a","kind":"agent"}`); status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	status1, body1 := post(t, srv, "/identities", "k2", `{"handle":"a","kind":"agent"}`)
	if status1 != http.StatusConflict {
		t.Fatalf("duplicate: %d %s", status1, body1)
	}
	status2, body2 := post(t, srv, "/identities", "k2", `{"handle":"a","kind":"agent"}`)
	if status2 != status1 || body2 != body1 {
		t.Fatalf("rejection replay differs: %d %s vs %d %s", status2, body2, status1, body1)
	}
}

// TestKeysScopeByOperation pins the contract note that client keys are
// never required to be globally unique across endpoints: a key already
// settled under another operation must not shadow this one.
func TestKeysScopeByOperation(t *testing.T) {
	srv, db := startAPI(t)
	if _, err := db.Exec(`INSERT INTO idempotency_keys (operation, key, status, body) VALUES ('POST /other', 'shared', 200, '{"foreign":true}')`); err != nil {
		t.Fatalf("seed foreign operation: %v", err)
	}
	status, body := post(t, srv, "/identities", "shared", `{"handle":"a","kind":"agent"}`)
	if status != http.StatusCreated {
		t.Fatalf("scoped key was shadowed by another operation: %d %s", status, body)
	}
}

// TestConcurrentSameKey races N identical requests: exactly one mutation
// applies and every caller receives the winner's response.
func TestConcurrentSameKey(t *testing.T) {
	srv, db := startAPI(t)
	const n = 8
	type result struct {
		status int
		body   string
	}
	var wg sync.WaitGroup
	results := make(chan result, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := post(t, srv, "/identities", "raced", `{"handle":"same","kind":"agent"}`)
			results <- result{status, body}
		}()
	}
	wg.Wait()
	close(results)
	var first *result
	for r := range results {
		if first == nil {
			first = &r
			continue
		}
		if r.status != first.status || r.body != first.body {
			t.Fatalf("divergent responses under one key: %d %s vs %d %s", r.status, r.body, first.status, first.body)
		}
	}
	if first.status != http.StatusCreated {
		t.Fatalf("expected 201 for all, got %d %s", first.status, first.body)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identities`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("same key applied %d times", count)
	}
}

// TestKeyed400sReplay pins that early rejections settle their key: a
// malformed body's 400 replays verbatim even when the retry is valid.
func TestKeyed400sReplay(t *testing.T) {
	srv, db := startAPI(t)
	status1, body1 := post(t, srv, "/identities", "k400", `{"handle":""}`)
	if status1 != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", status1, body1)
	}
	status2, body2 := post(t, srv, "/identities", "k400", `{"handle":"now-valid","kind":"agent"}`)
	if status2 != status1 || body2 != body1 {
		t.Fatalf("corrected retry under same key must replay the 400: got %d %s", status2, body2)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identities`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("settled 400 key still mutated: %d identities", count)
	}
}

// TestTrailingJSONRejected pins that a body with data after the first
// JSON value is malformed: 400, no mutation.
func TestTrailingJSONRejected(t *testing.T) {
	srv, db := startAPI(t)
	status, body := post(t, srv, "/identities", "ktrail", `{"handle":"a","kind":"agent"}{"extra":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("trailing JSON accepted: %d %s", status, body)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identities`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("trailing-JSON request mutated: %d identities", count)
	}
}
