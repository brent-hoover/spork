//go:build proof

// Package proof exercises kriya's tracker client against a REAL sutra.
//
// The unit tests' doubles honour idempotency keys the way sutra's source says
// it does. This checks that the reading was right — and the two contract bugs
// already found by reading (status vs state, /resubmit vs /revisions) were both
// invisible to a double that agreed with the misreading.
//
// Behind a build tag because it needs a server: `go test -tags proof
// ./internal/proof/ -v` with SUTRA_URL set.
package proof

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"kriya/internal/trackerclient"
)

func TestEveryCallKriyaMakesAgainstLiveSutra(t *testing.T) {
	base := os.Getenv("SUTRA_URL")
	if base == "" {
		t.Skip("SUTRA_URL unset")
	}
	c := trackerclient.New(base)
	ctx := context.Background()

	actor, err := c.CreateIdentity(ctx, "kriya-proof", "agent", "Kriya proof", "id-key-1")
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	projectRow, err := c.CreateProject(ctx, "PROOF", "Proof project", actor.ID, "proj-key-1")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	project := projectRow.ID

	epicRow, err := c.CreateIssue(ctx, project, "Build proof", "umbrella", actor.ID, "epic-key-1")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	epic := epicRow.ID
	spikeRow, err := c.CreateIssue(ctx, project, "spike: does this work", "risk", actor.ID, "spike-key-1")
	if err != nil {
		t.Fatalf("create spike: %v", err)
	}
	spike := spikeRow.ID
	dependentRow, err := c.CreateIssue(ctx, project, "build on the answer", "", actor.ID, "dep-key-1")
	if err != nil {
		t.Fatalf("create dependent: %v", err)
	}
	dependent := dependentRow.ID

	// The epic parents both; the spike BLOCKS the dependent. Risk-first rests
	// entirely on sutra honouring that relation kind.
	for _, rel := range []struct{ from, kind, to, key string }{
		{epic, "parent_of", spike, "rel-1"},
		{epic, "parent_of", dependent, "rel-2"},
		{spike, "blocks", dependent, "rel-3"},
	} {
		if err := c.AddRelation(ctx, rel.from, rel.kind, rel.to, actor.ID, rel.key); err != nil {
			t.Fatalf("relation %s %s %s: %v", rel.from, rel.kind, rel.to, err)
		}
	}

	// The ACTIVE listing, which completion detection reads. This is where
	// decoding "state" instead of "status" gave every issue a blank one.
	listing, err := c.ListIssues(ctx, project, []string{"open", "queued", "in-progress", "blocked"})
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	if listing.Watermark == "" {
		t.Error("the listing carries no feed watermark; a claim could not fence on it")
	}
	var sawSpike bool
	for _, i := range listing.Issues {
		if i.Status == "" {
			t.Errorf("issue %s decoded with an empty status", i.ID)
		}
		if i.ID == spike {
			sawSpike = true
		}
	}
	if !sawSpike {
		t.Errorf("the spike is not in the active listing: %+v", listing.Issues)
	}
	t.Logf("watermark=%s active=%d", listing.Watermark, len(listing.Issues))

	// The epic's subtree revision, which a completion claim fences on.
	got, err := c.GetIssue(ctx, epic)
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if got.Status == "" {
		t.Error("the epic decoded with an empty status")
	}
	t.Logf("epic status=%s subtree_revision=%d", got.Status, got.SubtreeRevision)

	proveDocuments(ctx, t, c, project, spike, actor.ID)
	proveFeed(ctx, t, c)
}

// proveDocuments checks the document deliverable path, and that a replayed key
// returns the ORIGINAL rather than appending beside it — which is what every
// crash-window test in the build assumes.
func proveDocuments(
	ctx context.Context, t *testing.T, c *trackerclient.Client,
	project, spike, actor string,
) {
	t.Helper()
	doc, err := c.CreateDocument(ctx, project, "Finding: does this work",
		spike, "# It works\n\nEvidence: ...\n", actor, "doc-key-1")
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	if doc.CurrentVersion == nil || *doc.CurrentVersion == "" {
		t.Fatalf("the document points at no version: %+v", doc)
	}
	t.Logf("document=%s version=%s", doc.ID, *doc.CurrentVersion)

	// The SAME key with DIFFERENT bytes. sutra must return the original: the
	// whole replay protocol rests on it, and a second version would mean the
	// review names something the human never saw.
	replay, err := c.CreateDocument(ctx, project, "Finding: does this work",
		spike, "# DIFFERENT BYTES\n", actor, "doc-key-1")
	if err != nil {
		t.Fatalf("replayed document: %v", err)
	}
	if replay.ID != doc.ID {
		t.Errorf("a replayed key created a second document: %s then %s", doc.ID, replay.ID)
	}
	if replay.CurrentVersion == nil || *replay.CurrentVersion != *doc.CurrentVersion {
		t.Errorf("a replayed key appended a version: %s then %v",
			*doc.CurrentVersion, replay.CurrentVersion)
	}

	review, err := c.CreateDocReview(ctx, spike, actor,
		"Finding", *doc.CurrentVersion, "review-key-1")
	if err != nil {
		t.Fatalf("create doc review: %v", err)
	}
	t.Logf("review=%s revision=%d state=%s", review.ID, review.Revision, review.State)

	again, err := c.CreateDocReview(ctx, spike, actor,
		"Finding", *doc.CurrentVersion, "review-key-1")
	if err != nil {
		t.Fatalf("replayed review: %v", err)
	}
	if again.ID != review.ID {
		t.Errorf("a replayed key opened a second review: %s then %s", review.ID, again.ID)
	}

	// And reading it back, which the approval poll does.
	read, err := c.GetReview(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if read.State == "" {
		t.Error("the review decoded with an empty state")
	}
	t.Logf("review read back state=%s revision=%d", read.State, read.Revision)
}

// proveFeed checks the event feed the work watcher consumes.
func proveFeed(ctx context.Context, t *testing.T, c *trackerclient.Client) {
	t.Helper()
	page, err := c.Events(ctx, "", "", 200)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	kinds := map[string]int{}
	for _, e := range page.Events {
		kinds[e.Kind]++
		if e.Subject == "" {
			t.Errorf("event %s has no subject; the watcher could not scope it", e.ID)
		}
	}
	t.Logf("feed events=%d next=%q kinds=%v", len(page.Events), page.NextCursor, kinds)
	if kinds["issue.created"] == 0 {
		t.Error("no issue.created events; the work watcher would see nothing")
	}
	// The cursor must be a decimal position: the claim's watermark is
	// compared against it numerically.
	if page.NextCursor == "" {
		t.Error("the feed reports no next cursor")
	}
}

// TestBlocksActuallyPreventsAPop is the load-bearing assumption of risk-first.
//
// kriya wires a blocking relation and then trusts sutra never to offer the
// blocked work. Assignment ORDER is only a nicety on top of that: if the
// relation did not hold, a queue-position accident would start dependent work
// before its risk was answered, and nothing in kriya would notice.
func TestBlocksActuallyPreventsAPop(t *testing.T) {
	base := os.Getenv("SUTRA_URL")
	if base == "" {
		t.Skip("SUTRA_URL unset")
	}
	c := trackerclient.New(base)
	ctx := context.Background()

	actor, err := c.CreateIdentity(ctx, "kriya-pop", "agent", "Kriya pop", "pop-id-1")
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	project, err := c.CreateProject(ctx, "POP", "Pop project", actor.ID, "pop-proj-1")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	spike, err := c.CreateIssue(ctx, project.ID, "spike: the risk", "", actor.ID, "pop-spike-1")
	if err != nil {
		t.Fatalf("create spike: %v", err)
	}
	blocked, err := c.CreateIssue(ctx, project.ID, "waits on the risk", "", actor.ID, "pop-blocked-1")
	if err != nil {
		t.Fatalf("create blocked: %v", err)
	}
	if err := c.AddRelation(ctx, spike.ID, "blocks", blocked.ID, actor.ID, "pop-rel-1"); err != nil {
		t.Fatalf("block: %v", err)
	}

	// Assigned in the WRONG order on purpose: the blocked ticket first. If the
	// relation did not hold, FIFO would hand it out before the spike.
	if err := c.AssignIssue(ctx, blocked.ID, actor.ID, actor.ID, "pop-assign-1"); err != nil {
		t.Fatalf("assign blocked: %v", err)
	}
	if err := c.AssignIssue(ctx, spike.ID, actor.ID, actor.ID, "pop-assign-2"); err != nil {
		t.Fatalf("assign spike: %v", err)
	}

	first, err := c.Pop(ctx, actor.ID, "pop-key-1")
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if first.Issue.ID == "" {
		t.Fatal("nothing was workable, though the spike is unblocked and assigned")
	}
	if first.Issue.ID == blocked.ID {
		t.Fatal("sutra offered BLOCKED work ahead of its blocker — risk-first " +
			"rests on this relation, and assignment order alone cannot save it")
	}
	if first.Issue.ID != spike.ID {
		t.Errorf("popped %q, expected the spike", first.Issue.Title)
	}
	t.Logf("popped %q with the blocked ticket assigned FIRST", first.Issue.Title)

	// And the blocked one is still not offered while its blocker is open.
	second, err := c.Pop(ctx, actor.ID, "pop-key-2")
	if err != nil {
		t.Fatalf("second pop: %v", err)
	}
	if second.Issue.ID == blocked.ID {
		t.Error("the blocked ticket was offered while its blocker is open")
	}
	t.Logf("second pop offered %q", second.Issue.Title)
}

// TestTheSubtreeFenceActuallyRefuses checks the fence a completion claim rests
// on. kriya captures a revision and later closes the epic naming it; if sutra
// accepted a stale one, a build could complete over work nobody reviewed —
// which is exactly the reopen-and-recomplete case where current state looks
// identical and only the revision moved.
//
// The review is REAL and APPROVED here. A first attempt at this test passed
// while the close was being refused for a malformed review id, which proves
// nothing about the fence at all.
func TestTheSubtreeFenceActuallyRefuses(t *testing.T) {
	base := os.Getenv("SUTRA_URL")
	if base == "" {
		t.Skip("SUTRA_URL unset")
	}
	c := trackerclient.New(base)
	ctx := context.Background()

	actor, err := c.CreateIdentity(ctx, "kriya-fence", "agent", "Kriya fence", "fence-id-1")
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	project, err := c.CreateProject(ctx, "FENCE", "Fence project", actor.ID, "fence-proj-1")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	epic, err := c.CreateIssue(ctx, project.ID, "Build fenced", "", actor.ID, "fence-epic-1")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}

	// A real, approved review over a real document: the close gate wants one,
	// and without it the fence is never reached.
	doc, err := c.CreateDocument(ctx, project.ID, "Completion report", epic.ID,
		"# done\n", actor.ID, "fence-doc-1")
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	review, err := c.CreateDocReview(ctx, epic.ID, actor.ID, "Build complete",
		*doc.CurrentVersion, "fence-review-1")
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	// A HUMAN identity. sutra refuses a verdict from an agent — the rule that
	// a human approves is enforced at the API, not merely by convention, which
	// is worth knowing: kriya could not fake an approval if it tried.
	human, err := c.CreateIdentity(ctx, "the-operator", "human", "Operator", "fence-human-1")
	if err != nil {
		t.Fatalf("create human: %v", err)
	}
	if err := approve(ctx, base, review.ID, human.ID, review.Revision); err != nil {
		t.Fatalf("approve: %v", err)
	}
	approved, err := c.GetReview(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if approved.State != "approved" {
		t.Fatalf("the review is %q, not approved", approved.State)
	}
	t.Logf("review approved at revision %d, verdict event %q",
		approved.Revision, approved.LatestVerdictEvent)

	captured, err := c.GetIssue(ctx, epic.ID)
	if err != nil {
		t.Fatalf("get epic: %v", err)
	}

	// The subtree MOVES after the capture: a child arrives.
	late, err := c.CreateIssue(ctx, project.ID, "late arrival", "", actor.ID, "fence-late-1")
	if err != nil {
		t.Fatalf("create late: %v", err)
	}
	if err := c.AddRelation(ctx, epic.ID, "parent_of", late.ID, actor.ID, "fence-rel-2"); err != nil {
		t.Fatalf("parent late: %v", err)
	}
	moved, err := c.GetIssue(ctx, epic.ID)
	if err != nil {
		t.Fatalf("get epic again: %v", err)
	}
	if moved.SubtreeRevision == captured.SubtreeRevision {
		t.Fatalf("attaching a child did not move the revision (%d); the fence "+
			"would never catch anything", moved.SubtreeRevision)
	}
	t.Logf("revision moved %d -> %d", captured.SubtreeRevision, moved.SubtreeRevision)

	// The STALE close. Refused — and the message must name the revision, not
	// something incidental, or this test is proving nothing again.
	err = c.CloseEpic(ctx, epic.ID, review.ID, approved.Revision,
		approved.LatestVerdictEvent, actor.ID, captured.SubtreeRevision, "fence-close-1")
	if err == nil {
		t.Fatal("sutra accepted a close on a STALE subtree revision — a build " +
			"could complete over work nobody reviewed")
	}
	if !strings.Contains(err.Error(), "subtree_revision") {
		t.Errorf("refused for something other than the fence: %v", err)
	}
	t.Logf("stale close refused by the fence: %v", err)

	// And the CURRENT revision is accepted, so the fence is a fence and not a
	// wall. The open child is what stops it now, which is sutra's other gate.
	err = c.CloseEpic(ctx, epic.ID, review.ID, approved.Revision,
		approved.LatestVerdictEvent, actor.ID, moved.SubtreeRevision, "fence-close-2")
	if err != nil && strings.Contains(err.Error(), "subtree_revision") {
		t.Errorf("the CURRENT revision was refused by the fence: %v", err)
	}
	t.Logf("close at the current revision: %v", err)
}

// approve sets a review's verdict. kriya never does this — a human does — so
// it is a raw call here rather than a client method nothing would use.
func approve(ctx context.Context, base, review, actor string, revision int) error {
	body := strings.NewReader(fmt.Sprintf(
		`{"verdict":"approved","revision":%d,"actor":%q}`, revision, actor))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/reviews/"+review+"/verdict", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "approve-"+review)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("verdict: status %d: %s", resp.StatusCode, out)
	}
	return nil
}
