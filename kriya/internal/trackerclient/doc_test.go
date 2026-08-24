package trackerclient_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestCreateDocumentSendsItsKeyAndDecodesTheVersion(t *testing.T) {
	c, got := serve(t, http.StatusCreated,
		`{"id":"doc-1","project":"p-1","title":"Completion report","current_version":"ver-1"}`)
	doc, err := c.CreateDocument(context.Background(), "p-1", "Completion report",
		"issue-7", "# Done", "actor-1", "key-1")
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	if doc.ID != "doc-1" {
		t.Errorf("decoded %+v", doc)
	}
	if doc.CurrentVersion == nil || *doc.CurrentVersion != "ver-1" {
		t.Error("the created version did not decode; a review has nothing to name")
	}
	if got.path != "/projects/p-1/documents" || got.key != "key-1" {
		t.Errorf("sent to %s with key %q", got.path, got.key)
	}
	if !strings.Contains(got.body, `"content":"# Done"`) {
		t.Errorf("the report content did not travel: %s", got.body)
	}
}

func TestADocumentWithNoIssueOmitsTheField(t *testing.T) {
	// sutra distinguishes absent from blank and rejects an explicit null.
	c, got := serve(t, http.StatusCreated, `{"id":"doc-1"}`)
	if _, err := c.CreateDocument(context.Background(), "p-1", "T", "", "body", "a", "k"); err != nil {
		t.Fatalf("create document: %v", err)
	}
	if strings.Contains(got.body, `"issue"`) {
		t.Errorf("an absent issue was sent anyway: %s", got.body)
	}
}

func TestSaveDocVersionAppendsUnderItsKey(t *testing.T) {
	c, got := serve(t, http.StatusCreated,
		`{"id":"ver-2","document":"doc-1","number":2}`)
	v, err := c.SaveDocVersion(context.Background(), "doc-1", "# Done again", "actor-1", "key-2")
	if err != nil {
		t.Fatalf("save version: %v", err)
	}
	if v.ID != "ver-2" || v.Number != 2 {
		t.Errorf("decoded %+v", v)
	}
	if got.path != "/documents/doc-1/versions" || got.key != "key-2" {
		t.Errorf("sent to %s with key %q", got.path, got.key)
	}
}

func TestADocReviewNamesItsVersionAndNoCommit(t *testing.T) {
	// sutra takes exactly ONE deliverable. A completion review that also sent
	// a branch would be rejected outright.
	c, got := serve(t, http.StatusCreated, `{"id":"review-1","revision":1}`)
	rv, err := c.CreateDocReview(context.Background(), "issue-7", "actor-1",
		"Build complete", "ver-1", "key-3")
	if err != nil {
		t.Fatalf("create doc review: %v", err)
	}
	if rv.ID != "review-1" {
		t.Errorf("decoded %+v", rv)
	}
	if !strings.Contains(got.body, `"doc_version":"ver-1"`) {
		t.Errorf("the deliverable did not travel: %s", got.body)
	}
	for _, forbidden := range []string{`"branch"`, `"commit"`} {
		if strings.Contains(got.body, forbidden) {
			t.Errorf("a document review sent %s: %s", forbidden, got.body)
		}
	}
	if got.key != "key-3" {
		t.Errorf("sent with key %q", got.key)
	}
}

func TestGetIssueDecodesTheSubtreeRevision(t *testing.T) {
	// The epic's revision is the fence a completion claim captures. It is not
	// in the active listing — an epic with no open children is not active
	// work, which is exactly the state completion is asking about.
	c, got := serve(t, http.StatusOK,
		`{"id":"epic-1","number":1,"title":"Build","status":"open","subtree_revision":7}`)
	issue, err := c.GetIssue(context.Background(), "epic-1")
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if issue.SubtreeRevision != 7 {
		t.Errorf("decoded revision %d", issue.SubtreeRevision)
	}
	if issue.Status != "open" {
		t.Errorf("decoded status %q", issue.Status)
	}
	if got.path != "/issues/epic-1" {
		t.Errorf("sent to %s", got.path)
	}
	if got.key != "" {
		t.Errorf("a read carried idempotency key %q", got.key)
	}
}
