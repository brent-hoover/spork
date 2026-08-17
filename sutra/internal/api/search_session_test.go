package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSearchSessionAndTextFindsIssueThroughNonMatchingThread pins the
// composition AC-search-session requires: session defines the scope and
// the text term narrows WITHIN it, applied to each candidate on its own
// terms. An issue reached through a session thread must be found when
// the ISSUE matches the term, even though the thread that links them
// does not — filtering threads by the term first dropped that issue
// before it was ever examined (review 1916).
func TestSearchSessionAndTextFindsIssueThroughNonMatchingThread(t *testing.T) {
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

	_, raw := post(t, srv, "/identities", "k-ident", `{"handle":"agent","kind":"agent"}`)
	actor := id(raw)
	status, raw := post(t, srv, "/projects", "k-proj", fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q}`, actor))
	if status != http.StatusCreated {
		t.Fatalf("create project: %d %s", status, raw)
	}
	project := id(raw)

	// The issue carries the term; the thread anchored to it does not.
	status, raw = post(t, srv, "/projects/"+project+"/issues", "k-issue",
		fmt.Sprintf(`{"title":"needle in the title","actor":%q}`, actor))
	if status != http.StatusCreated {
		t.Fatalf("create issue: %d %s", status, raw)
	}
	issue := id(raw)

	status, raw = post(t, srv, "/threads", "k-thread", fmt.Sprintf(
		`{"title":"unrelated chatter","session":"sess-1","issue":%q,"actor":%q,
		  "transcript":[{"role":"user","text":"nothing relevant here"}]}`, issue, actor))
	if status != http.StatusCreated {
		t.Fatalf("import thread: %d %s", status, raw)
	}

	status, body := get(t, srv, "/search?q=needle&session=sess-1")
	if status != http.StatusOK {
		t.Fatalf("search: %d %s", status, body)
	}
	var result struct {
		Issues []struct {
			ID string `json:"id"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("decode search: %v (%s)", err, body)
	}
	found := false
	for _, i := range result.Issues {
		if i.ID == issue {
			found = true
		}
	}
	if !found {
		t.Fatalf("issue matching the term was not reached through its session thread: %s", body)
	}
}

func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	return req(t, srv, http.MethodGet, path, "", "")
}
