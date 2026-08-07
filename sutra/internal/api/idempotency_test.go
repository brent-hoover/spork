package api_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestKeyedSyntacticallyMalformed400Replays pins that a parse-failure
// 400 settles its key: the valid retry replays the rejection.
func TestKeyedSyntacticallyMalformed400Replays(t *testing.T) {
	srv, db := startAPI(t)
	status1, body1 := post(t, srv, "/identities", "kparse", `{"handle": !!!`)
	if status1 != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", status1, body1)
	}
	status2, body2 := post(t, srv, "/identities", "kparse", `{"handle":"fine","kind":"agent"}`)
	if status2 != status1 || body2 != body1 {
		t.Fatalf("valid retry must replay the parse 400: got %d %s", status2, body2)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identities`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("settled parse-400 key still mutated: %d identities", count)
	}
}

// TestTrailingJSON400Replays pins the same for the trailing-data 400.
func TestTrailingJSON400Replays(t *testing.T) {
	srv, db := startAPI(t)
	status1, body1 := post(t, srv, "/identities", "ktrail2", `{"handle":"a","kind":"agent"}{"x":1}`)
	if status1 != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", status1, body1)
	}
	status2, body2 := post(t, srv, "/identities", "ktrail2", `{"handle":"a","kind":"agent"}`)
	if status2 != status1 || body2 != body1 {
		t.Fatalf("valid retry must replay the trailing-data 400: got %d %s", status2, body2)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identities`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("settled trailing-400 key still mutated: %d identities", count)
	}
}

// TestDuplicatePropertiesRejectOnEveryMutation pins that the ambiguity
// rule guards the whole API, not just imports: a transcript the server
// would store VERBATIM must not carry a repeated property, or exporting
// and re-importing it could not reproduce what was stored. Sibling
// objects repeating a name stay legal (review 1902).
func TestDuplicatePropertiesRejectOnEveryMutation(t *testing.T) {
	srv, db := startAPI(t)
	dup := `{"handle":"a","handle":"b","kind":"agent"}`
	status, body := post(t, srv, "/identities", "kdup", dup)
	if status != http.StatusBadRequest {
		t.Fatalf("duplicate property accepted: %d %s", status, body)
	}
	if !strings.Contains(body, "repeats property") {
		t.Fatalf("rejection does not name the cause: %s", body)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identities`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("ambiguous body mutated: %d identities", count)
	}
	// The rejection settles: the corrected retry replays the 400.
	status2, body2 := post(t, srv, "/identities", "kdup", `{"handle":"a","kind":"agent"}`)
	if status2 != status || body2 != body {
		t.Fatalf("corrected retry must replay the duplicate 400: %d %s", status2, body2)
	}
	// Repeats across SIBLING objects are ordinary valid JSON.
	if status, body := post(t, srv, "/identities", "ksib", `{"handle":"sib","kind":"agent"}`); status != http.StatusCreated {
		t.Fatalf("valid body rejected: %d %s", status, body)
	}
}

// TestOversizedBody400Replays pins that a body-read failure settles its
// key: the valid same-key retry replays the 400 without mutating.
func TestOversizedBody400Replays(t *testing.T) {
	api.SetBodyLimitForTest(1 << 10)
	t.Cleanup(func() { api.SetBodyLimitForTest(0) })
	srv, db := startAPI(t)
	huge := `{"handle":"` + strings.Repeat("x", 1<<11) + `","kind":"agent"}`
	status1, body1 := post(t, srv, "/identities", "kbig", huge)
	if status1 != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized body, got %d", status1)
	}
	status2, body2 := post(t, srv, "/identities", "kbig", `{"handle":"small","kind":"agent"}`)
	if status2 != status1 || body2 != body1 {
		t.Fatalf("valid retry must replay the oversize 400: got %d %s", status2, body2)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identities`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("settled oversize-400 key still mutated: %d identities", count)
	}
}

// TestEmptyCommentBodyAccepted pins that "" is a VALUE, not an absence:
// NewComment.body constrains no length and AC-comment-no-cap declares no
// minimum, so only omitting the property is an error (review 1926).
func TestEmptyCommentBodyAccepted(t *testing.T) {
	srv, _ := startAPI(t)
	id := func(body string) string {
		var v struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("decode id from %s: %v", body, err)
		}
		return v.ID
	}
	_, raw := post(t, srv, "/identities", "k-i", `{"handle":"a","kind":"human"}`)
	actor := id(raw)
	_, raw = post(t, srv, "/projects", "k-p", fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q}`, actor))
	project := id(raw)
	_, raw = post(t, srv, "/projects/"+project+"/issues", "k-iss", fmt.Sprintf(`{"title":"t","actor":%q}`, actor))
	issue := id(raw)

	status, body := post(t, srv, "/comments", "k-empty",
		fmt.Sprintf(`{"issue":%q,"author":%q,"body":""}`, issue, actor))
	if status != http.StatusCreated {
		t.Fatalf("empty comment body rejected: %d %s", status, body)
	}
	// Absence still rejects.
	status, body = post(t, srv, "/comments", "k-absent",
		fmt.Sprintf(`{"issue":%q,"author":%q}`, issue, actor))
	if status != http.StatusBadRequest {
		t.Fatalf("comment without a body accepted: %d %s", status, body)
	}
}

// TestImportResponseReportsStoredProject pins that the 201 describes
// what was PERSISTED, not the validation projection whose unread
// strings are sentinels. A wrong body here also replays wrong forever
// under the idempotency key (review 1936).
func TestImportResponseReportsStoredProject(t *testing.T) {
	srv, _ := startAPI(t)
	const (
		project = "01900000-0000-7000-8000-000000000000"
		actor   = "01900000-0000-7000-8000-00000000000f"
	)
	description := "a description long enough to be worth stripping"
	repoPath := "/somewhere/on/disk/that/matters"
	body := `{"project":{"id":"` + project + `","key":"IMP","name":"Imported",
			"description":"` + description + `","repo_path":"` + repoPath + `"},
		"identities":[{"id":"` + actor + `","handle":"importer","kind":"agent"}],
		"issues":[],"comments":[],"labels":[],"issue_relations":[],
		"documents":[],"threads":[],"reviews":[],"events":[]}`

	status, raw := post(t, srv, "/projects/import?actor="+actor, "k-import", body)
	if status != http.StatusCreated {
		t.Fatalf("import: %d %s", status, raw)
	}
	var got struct {
		Description *string `json:"description"`
		RepoPath    *string `json:"repo_path"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if got.Description == nil || *got.Description != description {
		t.Fatalf("import response description = %v, want %q", got.Description, description)
	}
	if got.RepoPath == nil || *got.RepoPath != repoPath {
		t.Fatalf("import response repo_path = %v, want %q", got.RepoPath, repoPath)
	}
	// The stored project agrees, and so does the replay.
	_, fetched := req(t, srv, http.MethodGet, "/projects/"+project, "", "")
	if !strings.Contains(fetched, description) {
		t.Fatalf("stored project lost its description: %s", fetched)
	}
	if _, replayed := post(t, srv, "/projects/import?actor="+actor, "k-import", body); replayed != raw {
		t.Fatalf("replay differs from the original response:\n%s\n%s", replayed, raw)
	}
}
