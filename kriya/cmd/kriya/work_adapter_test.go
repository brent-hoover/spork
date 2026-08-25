package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kriya/internal/planner"
	"kriya/internal/trackerclient"
)

func TestTheWorkFeedKeepsOnlyTheKindsThatCanReturnWork(t *testing.T) {
	// sutra's kind filter takes ONE value and the watcher cares about more
	// than one, so the feed is read unfiltered and narrowed here. A kind that
	// slipped through would advance the epoch for an assignment or a label.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"events":[
			{"id":"E1","kind":"issue.status-changed","subject":"issue-1"},
			{"id":"E2","kind":"issue.assigned","subject":"issue-1"},
			{"id":"E3","kind":"issue.labeled","subject":"issue-1"},
			{"id":"E4","kind":"issue.created","subject":"issue-2"},
			{"id":"E5","kind":"review.approved","subject":"review-1"}
		],"next_cursor":"c-9"}`)
	}))
	defer srv.Close()

	got, next, err := sutraWorkFeed{c: trackerclient.New(srv.URL)}.
		Since(t.Context(), "")
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if next != "c-9" {
		t.Errorf("the cursor came back %q", next)
	}
	if len(got) != 2 {
		t.Fatalf("kept %d events: %+v", len(got), got)
	}
	for i, want := range []string{planner.KindStatusChanged, planner.KindCreated} {
		if got[i].Kind != want {
			t.Errorf("event %d is %q, want %q", i, got[i].Kind, want)
		}
	}
	if got[0].Subject != "issue-1" || got[0].ID != "E1" {
		t.Errorf("the event decoded as %+v", got[0])
	}
}

func TestAnUnreachableFeedIsAnError(t *testing.T) {
	// "I could not read the feed" is not "nothing came back".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"code":"unavailable"}`)
	}))
	defer srv.Close()
	if _, _, err := (sutraWorkFeed{c: trackerclient.New(srv.URL)}).
		Since(t.Context(), ""); err == nil {
		t.Fatal("an unreachable feed read as empty")
	}
}

func TestTheIssueStateAdapterReadsTheLiveStatus(t *testing.T) {
	// A status-changed event carries no payload, so this read is what says
	// whether the change was a reopen.
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		_, _ = io.WriteString(w, `{"id":"issue-1","status":"open","subtree_revision":3}`)
	}))
	defer srv.Close()

	status, err := sutraIssueStates{c: trackerclient.New(srv.URL)}.
		Status(t.Context(), "issue-1")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "open" {
		t.Errorf("read status %q", status)
	}
	if !strings.HasSuffix(asked, "/issues/issue-1") {
		t.Errorf("asked %s", asked)
	}
}

func TestAnUnreadableIssueIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"code":"internal"}`)
	}))
	defer srv.Close()
	if _, err := (sutraIssueStates{c: trackerclient.New(srv.URL)}).
		Status(t.Context(), "issue-1"); err == nil {
		t.Fatal("an unreadable issue read as a blank status")
	}
}

func TestTheDocumentAdapterReportsAVersionlessDocument(t *testing.T) {
	// The review's deliverable IS the version. A document pointing at nothing
	// must surface as such rather than opening a review over nothing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"doc-1","title":"Report"}`)
	}))
	defer srv.Close()
	doc, version, err := sutraDocs{c: trackerclient.New(srv.URL), actor: "a"}.
		Create(t.Context(), "p-1", "Report", "issue-1", "# Done", "key-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if doc != "doc-1" {
		t.Errorf("decoded document %q", doc)
	}
	if version != "" {
		t.Errorf("a document with no current version reported %q", version)
	}
}

func TestTheCompletionReviewAdapterDecodesItsRevision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"review-1","revision":1}`)
	}))
	defer srv.Close()
	id, revision, err := sutraCompletionReviews{c: trackerclient.New(srv.URL), actor: "a"}.
		Create(t.Context(), "epic-1", "Build complete", "ver-1", "key-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id != "review-1" || revision != 1 {
		t.Errorf("decoded %q at revision %d", id, revision)
	}
}

func TestTheEpicAdapterPassesTheFenceThrough(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	if err := (sutraEpics{c: trackerclient.New(srv.URL), actor: "actor-1"}).
		Close(t.Context(), "epic-1", "review-1", 2, "event-9", 7, "close-key"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !strings.Contains(body, `"expected_subtree_revision":7`) {
		t.Errorf("the fence did not travel: %s", body)
	}
	if !strings.Contains(body, `"actor":"actor-1"`) {
		t.Errorf("the actor did not travel: %s", body)
	}
}

func TestTheLiveIssueAdapterAsksOnlyForActiveWork(t *testing.T) {
	// Paging a long-lived project's completed history to find the active few
	// is work neither side needs to do.
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"feed_watermark":"w-1","issues":[
			{"id":"i-1","status":"blocked","subtree_revision":4}]}`)
	}))
	defer srv.Close()

	rows, watermark, err := sutraIssues{c: trackerclient.New(srv.URL)}.
		Active(t.Context(), "p-1")
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if watermark != "w-1" {
		t.Errorf("watermark %q", watermark)
	}
	if len(rows) != 1 || rows[0].Status != "blocked" || rows[0].SubtreeRevision != 4 {
		t.Errorf("decoded %+v", rows)
	}
	for _, want := range []string{"open", "queued", "in-progress", "blocked"} {
		if !strings.Contains(query, "status="+want) {
			t.Errorf("the query %q omits %s", query, want)
		}
	}
	if strings.Contains(query, "status=complete") {
		t.Errorf("completed history was asked for: %s", query)
	}
}

func TestAnUnreachableIssueListIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"code":"bad-gateway"}`)
	}))
	defer srv.Close()
	if _, _, err := (sutraIssues{c: trackerclient.New(srv.URL)}).
		Active(t.Context(), "p-1"); err == nil {
		t.Fatal("an unreachable tracker read as an empty project")
	}
}
