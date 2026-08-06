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
// rejects with 400 via SQL length() — and that within the bound the
// same request diffs fine.
func TestDiffSizeBoundRejectsBeforeLoad(t *testing.T) {
	srv, _ := startAPI(t)
	docID := seedDocument(t, srv, "one\n", "two\n")

	orig := docs.MaxDiffInput
	docs.MaxDiffInput = 4
	t.Cleanup(func() { docs.MaxDiffInput = orig })

	resp, err := srv.Client().Get(srv.URL + "/documents/" + docID + "/diff?from=1&to=2")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize diff must 400, got %d", resp.StatusCode)
	}

	docs.MaxDiffInput = orig
	resp, err = srv.Client().Get(srv.URL + "/documents/" + docID + "/diff?from=1&to=2")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("in-bound diff must 200, got %d", resp.StatusCode)
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
