package trackerclient_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestResubmitDocReviewNamesTheNewVersionAndItsFences(t *testing.T) {
	// A revised FINDING. The deliverable is a new document version, and the
	// fences are the same a code resubmission carries: sutra refuses if the
	// review moved on, so a replay cannot advance a revision twice.
	c, got := serve(t, http.StatusOK, `{"id":"review-1","revision":3}`)
	revision, err := c.ResubmitDocReview(context.Background(), "review-1", "actor-1",
		"Finding: headless?", "ver-2", 2, "event-9", "doc-key-2")
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if revision != 3 {
		t.Errorf("advanced to revision %d", revision)
	}
	if got.path != "/reviews/review-1/resubmit" {
		t.Errorf("sent to %s", got.path)
	}
	if got.key != "doc-key-2" {
		t.Errorf("sent with key %q", got.key)
	}
	for _, want := range []string{
		`"doc_version":"ver-2"`, `"expected_revision":2`,
		`"expected_verdict_event":"event-9"`,
	} {
		if !strings.Contains(got.body, want) {
			t.Errorf("the request omits %s: %s", want, got.body)
		}
	}
	// A document review carries no code deliverable; sutra takes exactly one.
	for _, forbidden := range []string{`"branch"`, `"commit"`} {
		if strings.Contains(got.body, forbidden) {
			t.Errorf("a document resubmission sent %s: %s", forbidden, got.body)
		}
	}
}

func TestARefusedFindingResubmissionIsAnError(t *testing.T) {
	c, _ := serve(t, http.StatusConflict,
		`{"code":"conflict","message":"expected revision 2, current is 3"}`)
	if _, err := c.ResubmitDocReview(context.Background(), "review-1", "a",
		"s", "ver-2", 2, "event-9", "k"); err == nil {
		t.Fatal("a refused resubmission read as an advance")
	}
}
