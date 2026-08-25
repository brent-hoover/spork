package trackerclient_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"kriya/internal/trackerclient"
)

func TestCloseEpicSendsTheSubtreeFence(t *testing.T) {
	// The fence is what makes a completion claim provable rather than
	// hopeful: a child that reopened and recompleted before kriya consumed
	// either event leaves current state identical and the revision advanced,
	// which is exactly what a human's approval did not cover.
	c, got := serve(t, http.StatusOK, `{}`)
	err := c.CloseEpic(context.Background(), "epic-1", "review-1", 2,
		"event-9", "actor-1", 7, "close-key")
	if err != nil {
		t.Fatalf("close epic: %v", err)
	}
	if got.path != "/issues/epic-1/status" || got.key != "close-key" {
		t.Errorf("sent to %s with key %q", got.path, got.key)
	}
	for _, want := range []string{
		`"status":"complete"`, `"review":"review-1"`, `"review_revision":2`,
		`"review_verdict_event":"event-9"`, `"expected_subtree_revision":7`,
	} {
		if !strings.Contains(got.body, want) {
			t.Errorf("the request omits %s: %s", want, got.body)
		}
	}
}

func TestAStaleSubtreeRevisionSurfacesAsAConflict(t *testing.T) {
	// sutra rejects it, and kriya must see a refusal rather than a close —
	// the difference between "the claim is stale" and "the build is done".
	c, _ := serve(t, http.StatusConflict,
		`{"code":"conflict","message":"expected subtree_revision 7, current is 9"}`)
	err := c.CloseEpic(context.Background(), "epic-1", "review-1", 2,
		"event-9", "actor-1", 7, "close-key")
	if err == nil {
		t.Fatal("a refused close read as a successful one")
	}
	var apiErr *trackerclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("the refusal did not surface as a conflict: %v", err)
	}
}
