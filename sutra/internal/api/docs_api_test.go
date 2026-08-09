package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sutra/internal/docs"
)

// seedDocument creates an author, project, and document with two
// versions, returning the document id.
func seedDocument(t *testing.T, srv *httptest.Server, v1, v2 string) string {
	t.Helper()
	status, body := post(t, srv, "/identities", "doc-author", `{"handle":"doc-author","kind":"human"}`)
	if status != http.StatusCreated {
		t.Fatalf("author: %d %s", status, body)
	}
	var author struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &author); err != nil {
		t.Fatalf("author body: %v", err)
	}
	status, body = post(t, srv, "/projects", "doc-proj", `{"key":"docs","name":"Docs","actor":"`+author.ID+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("project: %d %s", status, body)
	}
	var project struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &project); err != nil {
		t.Fatalf("project body: %v", err)
	}
	payload, err := json.Marshal(map[string]any{"title": "Spec", "content": v1, "author": author.ID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	status, body = post(t, srv, "/projects/"+project.ID+"/documents", "doc-1", string(payload))
	if status != http.StatusCreated {
		t.Fatalf("document: %d %s", status, body)
	}
	var doc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("doc body: %v", err)
	}
	payload, err = json.Marshal(map[string]any{"content": v2, "author": author.ID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	status, body = post(t, srv, "/documents/"+doc.ID+"/versions", "doc-v2", string(payload))
	if status != http.StatusCreated {
		t.Fatalf("version 2: %d %s", status, body)
	}
	return doc.ID
}

// TestDiffSizeBoundRejectsBeforeLoad pins that the combined-size bound
// rejects with 400 via SQL length(), and pins it AT the boundary rather
// than far past it. A bound that rejects at 4 bytes and accepts at 64
// MiB says nothing about which side of "exactly at the limit" the limit
// falls on, so the two legs below sit one byte apart.
func TestDiffSizeBoundRejectsBeforeLoad(t *testing.T) {
	srv, _ := startAPI(t)
	const v1, v2 = "one\n", "two\n"
	docID := seedDocument(t, srv, v1, v2)
	combined := int64(len(v1) + len(v2))

	orig := docs.MaxDiffInput
	t.Cleanup(func() { docs.MaxDiffInput = orig })

	diffStatus := func() int {
		t.Helper()
		resp, err := srv.Client().Get(srv.URL + "/documents/" + docID + "/diff?from=1&to=2")
		if err != nil {
			t.Fatalf("diff: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	docs.MaxDiffInput = combined - 1
	if got := diffStatus(); got != http.StatusBadRequest {
		t.Fatalf("one byte over the bound must 400, got %d", got)
	}
	docs.MaxDiffInput = combined
	if got := diffStatus(); got != http.StatusOK {
		t.Fatalf("exactly at the bound must 200, got %d", got)
	}
}

// TestTemplateUpdateWithNothingToChange pins that a PUT naming neither
// a new name nor new content is a no-op that returns the template, not
// a 500. Nothing in the request layer rejects an empty object — it only
// rejects explicit nulls — so the store's own guard is the only thing
// standing between `{}` and an UPDATE statement with an empty SET
// clause, which SQLite rejects as a syntax error.
func TestTemplateUpdateWithNothingToChange(t *testing.T) {
	srv, _ := startAPI(t)
	status, body := post(t, srv, "/templates", "tpl-1", `{"name":"tech-spec","content":"body"}`)
	if status != http.StatusCreated {
		t.Fatalf("create template: %d %s", status, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("create body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/templates/"+created.ID, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "tpl-noop")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty update must 200, got %d", resp.StatusCode)
	}
	var updated struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Name != "tech-spec" || updated.Content != "body" {
		t.Fatalf("empty update changed the template: %+v", updated)
	}
}

// TestLargeBodySpoolsAndSucceeds drives a body past largeBodyThreshold
// so it takes the spool path (unlinked temp file, streamed decode) and
// verifies the stored version round-trips intact.
func TestLargeBodySpoolsAndSucceeds(t *testing.T) {
	srv, _ := startAPI(t)
	large := strings.Repeat("spooled line of content\n", 200_000) // ~4.8 MiB
	docID := seedDocument(t, srv, "small\n", large)

	resp, err := srv.Client().Get(srv.URL + "/documents/" + docID + "/versions")
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var versions []struct {
		Number  int64  `json:"number"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	for _, v := range versions {
		if v.Number == 2 {
			if v.Content != large {
				t.Fatalf("spooled content corrupted: got %d bytes, want %d", len(v.Content), len(large))
			}
			return
		}
	}
	t.Fatalf("version 2 missing from %d versions", len(versions))
}
